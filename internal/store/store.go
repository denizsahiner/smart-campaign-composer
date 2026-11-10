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
	"maps"
	"slices"
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
	StatusScheduled = "scheduled"
	StatusApproved  = "approved"
	StatusCompleted = "completed"
)

var (
	ErrNotFound = errors.New("store: campaign not found")
	// ErrNotEditable means the campaign is scheduled, approved or completed.
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
	ID       int64
	Brief    string
	Status   string
	Audience *Audience // nil until the audience step succeeded
	// Contents is the draft per language, nil when there is none. It can hold a language
	// the audience no longer needs (after a refresh); only ContentLanguages count.
	Contents content.Set
	// ContentErrors are the stored drafts' validation errors, with the language in the
	// path ("en.sms"); nil when every draft is valid.
	ContentErrors []validation.FieldError
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ApprovedAt    *time.Time
	// ScheduledAt is when the messages go out, for a campaign approved for a later time;
	// nil when it was sent at once.
	ScheduledAt *time.Time
}

// ContentLanguages are the languages the campaign needs a draft in: those of its audience
// with a reachable recipient. Nil without an audience.
func (c Campaign) ContentLanguages() []string {
	if c.Audience == nil {
		return nil
	}
	return c.Audience.Counts.ContentLanguages()
}

// ContentReady reports whether every needed language has a draft that passes validation,
// which is what makes a campaign ready.
func (c Campaign) ContentReady() bool {
	langs := c.ContentLanguages()
	if len(langs) == 0 {
		return false
	}
	for _, l := range langs {
		if _, ok := c.Contents[l]; !ok || len(content.ErrorsFor(c.ContentErrors, l)) > 0 {
			return false
		}
	}
	return true
}

// ContentOptions are the validation options of the needed languages.
func (c Campaign) ContentOptions() content.LanguageOptions {
	o := content.LanguageOptions{}
	for _, l := range c.ContentLanguages() {
		o[l] = content.Options{LongestFirstName: c.Audience.Counts.Languages[l].LongestFirstName}
	}
	return o
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

// campaignColumns reads a campaign row with its drafts, as one JSON object keyed by
// language.
const campaignColumns = `id, brief, status, segment_rule, audience_as_of, audience_total_count,
	sms_audience_count, email_audience_count, longest_first_name, language_counts, unsupported_criteria,
	unsupported_acknowledged_at, content_language, created_at, updated_at, approved_at, scheduled_at,
	(SELECT jsonb_object_agg(cc.language, jsonb_build_object('sms', cc.sms, 'email_subject', cc.email_subject,
		'email_body', cc.email_body, 'errors', cc.errors))
	 FROM campaign_contents cc WHERE cc.campaign_id = campaigns.id)`

type storedContent struct {
	content.Content
	Errors []validation.FieldError `json:"errors"`
}

func scanCampaign(row pgx.Row) (Campaign, error) {
	var (
		c                          Campaign
		seg                        []byte
		asOf, ackAt                *time.Time
		total, sms, email, longest *int
		langCounts                 map[string]segment.LanguageCounts
		unsupported                []string
		legacyLang                 string
		contents                   map[string]storedContent
	)
	err := row.Scan(&c.ID, &c.Brief, &c.Status, &seg, &asOf, &total, &sms, &email, &longest, &langCounts,
		&unsupported, &ackAt, &legacyLang, &c.CreatedAt, &c.UpdatedAt, &c.ApprovedAt, &c.ScheduledAt, &contents)
	if errors.Is(err, pgx.ErrNoRows) {
		return Campaign{}, ErrNotFound
	}
	if err != nil {
		return Campaign{}, err
	}
	if seg != nil {
		c.Audience = &Audience{
			Segment: seg,
			AsOf:    *asOf,
			Counts: segment.Counts{Total: *total, SMS: *sms, Email: *email, LongestFirstName: *longest,
				Languages: langCounts},
			Unsupported:      unsupported,
			UnsupportedAckAt: ackAt,
		}
		if langCounts == nil {
			// Counted before per-language counts existed: everyone was written to in one
			// language.
			c.Audience.Counts.Languages = map[string]segment.LanguageCounts{legacyLang: {
				Total: *total, SMS: *sms, Email: *email, LongestFirstName: *longest}}
		}
	}
	for _, l := range slices.Sorted(maps.Keys(contents)) {
		if c.Contents == nil {
			c.Contents = content.Set{}
		}
		c.Contents[l] = contents[l].Content
		c.ContentErrors = append(c.ContentErrors, content.Prefix(l, contents[l].Errors)...)
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
// audience invalidates the drafts (their lengths, languages and wording depend on the
// segment) and any acknowledgement, so the campaign goes back to draft. seg is nil when
// the audience step failed: the brief is kept, the audience cleared.
func (s *Store) SetAudience(ctx context.Context, id int64, brief string, seg *segment.Segment,
	counts segment.Counts, asOf time.Time, unsupported []string) error {

	var segJSON, langJSON []byte
	var asOfArg *time.Time
	if seg != nil {
		var err error
		if segJSON, err = json.Marshal(seg); err != nil {
			return err
		}
		if langJSON, err = json.Marshal(counts.Languages); err != nil {
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
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE campaigns SET
				brief = $2, segment_rule = $3, audience_as_of = $4,
				audience_total_count = $5, sms_audience_count = $6, email_audience_count = $7,
				longest_first_name = $8, language_counts = $9, unsupported_criteria = $10,
				unsupported_acknowledged_at = NULL, status = 'draft', updated_at = now()
			WHERE id = $1 AND status IN ('draft', 'ready')`,
			id, brief, segJSON, asOfArg, nullIfNoSeg(counts.Total), nullIfNoSeg(counts.SMS),
			nullIfNoSeg(counts.Email), nullIfNoSeg(counts.LongestFirstName), langJSON, unsupported)
		if err := s.editResult(ctx, id, tag.RowsAffected(), err); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM campaign_contents WHERE campaign_id = $1`, id)
		return err
	})
}

// AcknowledgeUnsupported records that the marketer has seen the unsupported criteria.
func (s *Store) AcknowledgeUnsupported(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE campaigns SET unsupported_acknowledged_at = now(), updated_at = now()
		WHERE id = $1 AND status IN ('draft', 'ready') AND segment_rule IS NOT NULL`, id)
	return s.editResult(ctx, id, tag.RowsAffected(), err)
}

// SetContents stores drafts for the languages in set, leaving the other languages'
// drafts as they are. errs are the drafts' validation errors with the language in the
// path. The campaign becomes ready when every language it needs has a valid draft, and
// goes back to draft otherwise.
func (s *Store) SetContents(ctx context.Context, id int64, set content.Set, errs []validation.FieldError) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := s.lockEditable(ctx, tx, id); err != nil {
			return err
		}
		for _, l := range set.Languages() {
			c := set[l]
			var errsArg any // SQL NULL when valid, not JSON null
			if e := content.ErrorsFor(errs, l); len(e) > 0 {
				errsArg = e
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO campaign_contents (campaign_id, language, sms, email_subject, email_body, errors)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (campaign_id, language) DO UPDATE SET sms = $3, email_subject = $4,
					email_body = $5, errors = $6, updated_at = now()`,
				id, l, c.SMS, c.EmailSubject, c.EmailBody, errsArg); err != nil {
				return err
			}
		}
		return syncStatus(ctx, tx, id)
	})
}

// RefreshAudience recounts an unchanged segment for a new reference time. The drafts and
// the acknowledgement are kept; contentErrs are the stored drafts re-validated against
// the new counts (language in the path; nil when all valid), which with the languages
// now needed decides between ready and draft.
func (s *Store) RefreshAudience(ctx context.Context, id int64, counts segment.Counts, asOf time.Time, contentErrs []validation.FieldError) error {
	langJSON, err := json.Marshal(counts.Languages)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE campaigns SET
				audience_as_of = $2, audience_total_count = $3, sms_audience_count = $4,
				email_audience_count = $5, longest_first_name = $6, language_counts = $7, updated_at = now()
			WHERE id = $1 AND status IN ('draft', 'ready') AND segment_rule IS NOT NULL`,
			id, asOf, counts.Total, counts.SMS, counts.Email, counts.LongestFirstName, langJSON)
		if err := s.editResult(ctx, id, tag.RowsAffected(), err); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT language FROM campaign_contents WHERE campaign_id = $1`, id)
		if err != nil {
			return err
		}
		langs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, l := range langs {
			var errsArg any
			if e := content.ErrorsFor(contentErrs, l); len(e) > 0 {
				errsArg = e
			}
			if _, err := tx.Exec(ctx, `UPDATE campaign_contents SET errors = $3 WHERE campaign_id = $1 AND language = $2`,
				id, l, errsArg); err != nil {
				return err
			}
		}
		return syncStatus(ctx, tx, id)
	})
}

// lockEditable locks an editable campaign with an audience for the transaction, or says
// why it can't be changed.
func (s *Store) lockEditable(ctx context.Context, tx pgx.Tx, id int64) error {
	tag, err := tx.Exec(ctx, `
		SELECT 1 FROM campaigns WHERE id = $1 AND status IN ('draft', 'ready') AND segment_rule IS NOT NULL
		FOR UPDATE`, id)
	return s.editResult(ctx, id, tag.RowsAffected(), err)
}

// syncStatus sets ready or draft from the drafts the transaction left behind.
func syncStatus(ctx context.Context, tx pgx.Tx, id int64) error {
	c, err := scanCampaign(tx.QueryRow(ctx, `SELECT `+campaignColumns+` FROM campaigns WHERE id = $1`, id))
	if err != nil {
		return err
	}
	status := StatusDraft
	if c.ContentReady() {
		status = StatusReady
	}
	_, err = tx.Exec(ctx, `UPDATE campaigns SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	return err
}

func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
