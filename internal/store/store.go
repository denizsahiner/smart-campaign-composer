// Package store persists campaigns and the LLM call log.
//
// State changes are conditional updates (WHERE status IN ...), so a campaign that has
// been approved can't have its audience or content changed underneath the queue, even by
// concurrent requests.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denizsahiner/smart-campaign-composer/internal/compose"
	"github.com/denizsahiner/smart-campaign-composer/internal/content"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

const (
	StatusDraft     = "draft"
	StatusReady     = "ready"
	StatusApproved  = "approved"
	StatusCompleted = "completed"
)

var (
	ErrNotFound = errors.New("store: campaign not found")
	// ErrNotEditable means the campaign is approved or completed.
	ErrNotEditable = errors.New("store: campaign can no longer be changed")
)

// Audience is the stored result of the audience step.
type Audience struct {
	Segment          json.RawMessage // validated, normalized segment; re-parse before use
	AsOf             time.Time
	Counts           segment.Counts
	Unsupported      []string
	UnsupportedAckAt *time.Time
}

type Campaign struct {
	ID            int64
	Brief         string
	Status        string
	Audience      *Audience // nil until the audience step succeeded
	Content       *content.Content
	ContentErrors []validation.FieldError // errors of the stored draft; nil when valid
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ApprovedAt    *time.Time
}

// NeedsAcknowledgement reports whether unsupported criteria still block the messages step.
func (c Campaign) NeedsAcknowledgement() bool {
	return c.Audience != nil && len(c.Audience.Unsupported) > 0 && c.Audience.UnsupportedAckAt == nil
}

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Create(ctx context.Context, brief string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO campaigns (brief) VALUES ($1) RETURNING id`, brief).Scan(&id)
	return id, err
}

const campaignColumns = `id, brief, status, segment_rule, audience_as_of, audience_total_count,
	sms_audience_count, email_audience_count, longest_first_name, unsupported_criteria,
	unsupported_acknowledged_at, sms_content, email_subject, email_body, content_errors,
	created_at, updated_at, approved_at`

func scanCampaign(row pgx.Row) (Campaign, error) {
	var (
		c                          Campaign
		seg                        []byte
		asOf, ackAt                *time.Time
		total, sms, email, longest *int
		unsupported                []string
		sMS, subject, body         *string
		contentErrs                []validation.FieldError
	)
	err := row.Scan(&c.ID, &c.Brief, &c.Status, &seg, &asOf, &total, &sms, &email, &longest,
		&unsupported, &ackAt, &sMS, &subject, &body, &contentErrs, &c.CreatedAt, &c.UpdatedAt, &c.ApprovedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Campaign{}, ErrNotFound
	}
	if err != nil {
		return Campaign{}, err
	}
	if seg != nil {
		c.Audience = &Audience{
			Segment:          seg,
			AsOf:             *asOf,
			Counts:           segment.Counts{Total: *total, SMS: *sms, Email: *email, LongestFirstName: *longest},
			Unsupported:      unsupported,
			UnsupportedAckAt: ackAt,
		}
	}
	if sMS != nil {
		c.Content = &content.Content{SMS: *sMS, EmailSubject: *subject, EmailBody: *body}
		c.ContentErrors = contentErrs
	}
	return c, nil
}

func (s *Store) Get(ctx context.Context, id int64) (Campaign, error) {
	return scanCampaign(s.pool.QueryRow(ctx, `SELECT `+campaignColumns+` FROM campaigns WHERE id = $1`, id))
}

// List returns the most recent campaigns first.
func (s *Store) List(ctx context.Context, limit int) ([]Campaign, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+campaignColumns+` FROM campaigns ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Campaign
	for rows.Next() {
		c, err := scanCampaign(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetAudience stores a new brief and its audience, replacing any previous one. A new
// audience invalidates the draft (its lengths and wording depend on the segment) and
// any acknowledgement, so the campaign goes back to draft. seg is nil when the audience
// step failed: the brief is kept, the audience cleared.
func (s *Store) SetAudience(ctx context.Context, id int64, brief string, seg *segment.Segment,
	counts segment.Counts, asOf time.Time, unsupported []string) error {

	var segJSON []byte
	var asOfArg *time.Time
	if seg != nil {
		var err error
		if segJSON, err = json.Marshal(seg); err != nil {
			return err
		}
		asOfArg = &asOf
	}
	if unsupported == nil {
		unsupported = []string{}
	}
	nullIfNoSeg := func(v int) *int {
		if seg == nil {
			return nil
		}
		return &v
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE campaigns SET
			brief = $2, segment_rule = $3, audience_as_of = $4,
			audience_total_count = $5, sms_audience_count = $6, email_audience_count = $7,
			longest_first_name = $8, unsupported_criteria = $9, unsupported_acknowledged_at = NULL,
			sms_content = NULL, email_subject = NULL, email_body = NULL, content_errors = NULL,
			status = 'draft', updated_at = now()
		WHERE id = $1 AND status IN ('draft', 'ready')`,
		id, brief, segJSON, asOfArg, nullIfNoSeg(counts.Total), nullIfNoSeg(counts.SMS),
		nullIfNoSeg(counts.Email), nullIfNoSeg(counts.LongestFirstName), unsupported)
	return s.editResult(ctx, id, tag.RowsAffected(), err)
}

// AcknowledgeUnsupported records that the marketer has seen the unsupported criteria.
func (s *Store) AcknowledgeUnsupported(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE campaigns SET unsupported_acknowledged_at = now(), updated_at = now()
		WHERE id = $1 AND status IN ('draft', 'ready') AND segment_rule IS NOT NULL`, id)
	return s.editResult(ctx, id, tag.RowsAffected(), err)
}

// SetContent stores a draft. With no errors the campaign becomes ready (approvable);
// with errors it stays a draft and the errors are kept for the UI.
func (s *Store) SetContent(ctx context.Context, id int64, c content.Content, errs []validation.FieldError) error {
	// An untyped nil is sent as SQL NULL; a nil slice would be stored as JSON null.
	status := StatusReady
	var errsArg any
	if len(errs) > 0 {
		status, errsArg = StatusDraft, errs
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE campaigns SET sms_content = $2, email_subject = $3, email_body = $4,
			content_errors = $5, status = $6, updated_at = now()
		WHERE id = $1 AND status IN ('draft', 'ready') AND segment_rule IS NOT NULL`,
		id, c.SMS, c.EmailSubject, c.EmailBody, errsArg, status)
	return s.editResult(ctx, id, tag.RowsAffected(), err)
}

// editResult turns "0 rows updated" into the reason: no such campaign, or not editable.
func (s *Store) editResult(ctx context.Context, id int64, affected int64, err error) error {
	if err != nil || affected == 1 {
		return err
	}
	c, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if c.Status == StatusApproved || c.Status == StatusCompleted {
		return ErrNotEditable
	}
	return fmt.Errorf("store: campaign %d has no audience", id)
}

// RecordCall implements compose.Recorder: one llm_calls row per model call.
func (s *Store) RecordCall(ctx context.Context, c compose.Call) error {
	var raw, model, callErr *string
	if c.Raw != nil {
		r := string(c.Raw)
		raw = &r
	}
	if c.Model != "" {
		model = &c.Model
	}
	if c.Error != "" {
		callErr = &c.Error
	}
	var verrs any // SQL NULL when valid, not JSON null
	if len(c.ValidationErrors) > 0 {
		verrs = c.ValidationErrors
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO llm_calls (campaign_id, step, prompt_version, raw_response, validation_errors,
			repair_attempt, transport_attempt, input_tokens, output_tokens, latency_ms, model, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		c.CampaignID, string(c.Step), c.PromptVersion, raw, verrs, c.RepairAttempt, c.TransportAttempt,
		c.InputTokens, c.OutputTokens, c.Latency.Milliseconds(), model, callErr)
	return err
}
