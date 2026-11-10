package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denizsahiner/smart-campaign-composer/internal/compose"
	"github.com/denizsahiner/smart-campaign-composer/internal/content"
	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/logging"
	"github.com/denizsahiner/smart-campaign-composer/internal/prompt"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
	"github.com/denizsahiner/smart-campaign-composer/internal/store"
	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

const (
	maxBriefRunes    = 2000
	maxFeedbackRunes = 500
)

type audienceView struct {
	Rules       []segment.Rule `json:"rules"`
	Description []string       `json:"description"`
	Reach       segment.Counts `json:"reach"`
	// ContentLanguages are the languages the messages must be written in: those with a
	// reachable recipient (segment.Counts.ContentLanguages).
	ContentLanguages        []string  `json:"content_languages"`
	AsOf                    time.Time `json:"as_of"`
	UnsupportedCriteria     []string  `json:"unsupported_criteria"`
	UnsupportedAcknowledged bool      `json:"unsupported_acknowledged"`
}

type campaignView struct {
	ID       int64         `json:"id"`
	Brief    string        `json:"brief"`
	Status   string        `json:"status"`
	Audience *audienceView `json:"audience"`
	// Contents is the draft per language, null when there is none; ContentErrors carry
	// the language in the path ("en.sms").
	Contents      content.Set             `json:"contents"`
	ContentErrors []validation.FieldError `json:"content_errors,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
	ApprovedAt    *time.Time              `json:"approved_at"`
	ScheduledAt   *time.Time              `json:"scheduled_at"`
}

func (s *Server) view(c store.Campaign) (campaignView, error) {
	v := campaignView{
		ID: c.ID, Brief: c.Brief, Status: c.Status, Contents: c.Contents, ContentErrors: c.ContentErrors,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, ApprovedAt: c.ApprovedAt,
		ScheduledAt: c.ScheduledAt,
	}
	if a := c.Audience; a != nil {
		seg, err := s.validator.Parse(a.Segment)
		if err != nil {
			return campaignView{}, fmt.Errorf("stored segment of campaign %d no longer validates: %w", c.ID, err)
		}
		v.Audience = &audienceView{
			Rules: seg.Rules, Description: seg.Describe(), Reach: a.Counts, AsOf: a.AsOf,
			ContentLanguages:    nonNil(c.ContentLanguages()),
			UnsupportedCriteria: a.Unsupported, UnsupportedAcknowledged: a.UnsupportedAckAt != nil,
		}
	}
	return v, nil
}

// audienceResult is returned by the endpoints that run the audience step. A failed step
// is not an HTTP error: the request was fine, the outcome is reported in AudienceError
// so the UI can show it next to the brief.
type audienceResult struct {
	Campaign      campaignView `json:"campaign"`
	AudienceError *apiError    `json:"audience_error,omitempty"`
}

func (s *Server) listCampaigns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
		limit = v
	}
	cs, err := s.store.List(r.Context(), limit)
	if err != nil {
		internalError(w, r, err)
		return
	}
	out := make([]campaignView, 0, len(cs))
	for _, c := range cs {
		v, err := s.view(c)
		if err != nil {
			internalError(w, r, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaigns": out})
}

func (s *Server) getCampaign(w http.ResponseWriter, r *http.Request) {
	c, ok := s.campaign(w, r)
	if !ok {
		return
	}
	v, err := s.view(c)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type briefRequest struct {
	Brief string `json:"brief"`
}

// readBrief decodes and checks {"brief": "..."}.
func readBrief(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req briefRequest
	if !decode(w, r, &req) {
		return "", false
	}
	brief := strings.TrimSpace(req.Brief)
	if n := utf8.RuneCountInString(brief); n == 0 || n > maxBriefRunes {
		writeError(w, http.StatusUnprocessableEntity, "invalid_brief",
			fmt.Sprintf("The brief must be 1–%d characters.", maxBriefRunes),
			validation.FieldError{Path: "brief", Code: "length", Message: fmt.Sprintf("must be 1–%d characters", maxBriefRunes)})
		return "", false
	}
	return brief, true
}

func (s *Server) createCampaign(w http.ResponseWriter, r *http.Request) {
	brief, ok := readBrief(w, r)
	if !ok {
		return
	}
	id, err := s.store.Create(r.Context(), brief)
	if err != nil {
		internalError(w, r, err)
		return
	}
	logging.Add(r.Context(), "campaign_id", id)
	s.lock(id) // new campaign: nobody else can hold it yet
	defer s.unlock(id)
	s.respondAudience(w, r, id, brief, http.StatusCreated)
}

func (s *Server) rerunAudience(w http.ResponseWriter, r *http.Request) {
	c, ok := s.campaign(w, r)
	if !ok {
		return
	}
	brief, ok := readBrief(w, r)
	if !ok {
		return
	}
	if !editable(c) {
		writeNotEditable(w)
		return
	}
	if !s.lock(c.ID) {
		writeBusy(w)
		return
	}
	defer s.unlock(c.ID)
	s.respondAudience(w, r, c.ID, brief, http.StatusOK)
}

// respondAudience runs the audience step, stores the outcome and writes the result.
func (s *Server) respondAudience(w http.ResponseWriter, r *http.Request, id int64, brief string, status int) {
	ctx := r.Context()
	a, err := s.composer.Audience(ctx, id, brief, s.validator)
	audErr := audienceError(err, a)

	switch {
	case err == nil:
		asOf := s.now()
		counts, cerr := segment.Count(ctx, s.pool, a.Segment, asOf)
		if cerr != nil {
			internalError(w, r, cerr)
			return
		}
		err = s.store.SetAudience(ctx, id, brief, &a.Segment, counts, asOf, a.Unsupported)
	case errors.Is(err, prompt.ErrUnmappable), errors.As(err, new(*compose.ExhaustedError)):
		// The brief is the problem: keep the new brief, drop the old audience.
		err = s.store.SetAudience(ctx, id, brief, nil, segment.Counts{}, time.Time{}, a.Unsupported)
	case ctx.Err() != nil:
		return // the client went away
	default:
		// Claude unavailable, refusal: nothing learned about the brief, leave state as is.
		err = nil
	}
	if errors.Is(err, store.ErrNotEditable) {
		writeNotEditable(w)
		return
	}
	if err != nil {
		internalError(w, r, err)
		return
	}

	c, err := s.store.Get(ctx, id)
	if err != nil {
		internalError(w, r, err)
		return
	}
	v, err := s.view(c)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, status, audienceResult{Campaign: v, AudienceError: audErr})
}

// audienceError describes a failed audience step for the UI; nil on success.
func audienceError(err error, a prompt.Audience) *apiError {
	var exhausted *compose.ExhaustedError
	var refusal *llm.RefusalError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, prompt.ErrUnmappable):
		return &apiError{Code: "unmappable", Message: "Nothing in the brief can be expressed as an audience: " + strings.Join(a.Unsupported, "; ")}
	case errors.As(err, &exhausted):
		return &apiError{Code: "invalid_audience", Message: "Claude couldn't build a valid audience from this brief. Please rephrase it.", Errors: exhausted.Errors}
	case errors.Is(err, compose.ErrUnavailable):
		return &apiError{Code: "llm_unavailable", Message: "Claude is busy or unreachable right now. Please try again in a moment."}
	case errors.As(err, &refusal):
		return &apiError{Code: "llm_refused", Message: "Claude declined this brief. Please rephrase it."}
	default:
		return &apiError{Code: "llm_failed", Message: "Building the audience failed. Please try again."}
	}
}

func (s *Server) acknowledge(w http.ResponseWriter, r *http.Request) {
	c, ok := s.campaign(w, r)
	if !ok {
		return
	}
	if !editable(c) {
		writeNotEditable(w)
		return
	}
	if c.Audience == nil {
		writeError(w, http.StatusConflict, "no_audience", "The campaign has no audience yet.")
		return
	}
	if err := s.store.AcknowledgeUnsupported(r.Context(), c.ID); err != nil {
		if errors.Is(err, store.ErrNotEditable) {
			writeNotEditable(w)
			return
		}
		internalError(w, r, err)
		return
	}
	s.getCampaign(w, r)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
