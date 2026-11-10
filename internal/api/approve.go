package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/denizsahiner/smart-campaign-composer/internal/content"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
	"github.com/denizsahiner/smart-campaign-composer/internal/store"
	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

// fieldErrors extracts the field errors of a validation failure.
func fieldErrors(err error) []validation.FieldError {
	var ve *validation.Error
	if errors.As(err, &ve) {
		return ve.Errors
	}
	return []validation.FieldError{{Path: "$", Code: "invalid", Message: err.Error()}}
}

// refreshAudience recounts the stored segment for today, without calling Claude. It's
// how a stale audience (counted on an earlier day) becomes approvable again. The drafts
// and the acknowledgement are kept; each draft is re-validated in case the longest name
// among its language's recipients grew. A language that gained its first reachable
// recipient has no draft yet and keeps the campaign a draft until one is written.
func (s *Server) refreshAudience(w http.ResponseWriter, r *http.Request) {
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
	seg, err := s.validator.Parse(c.Audience.Segment)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if !s.lock(c.ID) {
		writeBusy(w)
		return
	}
	defer s.unlock(c.ID)

	asOf := s.now()
	counts, err := segment.Count(r.Context(), s.pool, seg, asOf)
	if err != nil {
		internalError(w, r, err)
		return
	}
	var contentErrs []validation.FieldError
	for _, l := range c.Contents.Languages() {
		opts := content.Options{LongestFirstName: counts.Languages[l].LongestFirstName}
		if _, verr := content.Validate(c.Contents[l], opts); verr != nil {
			contentErrs = append(contentErrs, content.Prefix(l, fieldErrors(verr))...)
		}
	}
	if err := s.store.RefreshAudience(r.Context(), c.ID, counts, asOf, contentErrs); err != nil {
		if errors.Is(err, store.ErrNotEditable) {
			writeNotEditable(w)
			return
		}
		internalError(w, r, err)
		return
	}
	s.getCampaign(w, r)
}

type approveRequest struct {
	// Contents has the final draft of every language the audience needs; others are
	// ignored.
	Contents content.Set `json:"contents"`
	// SendAt schedules the send for later; absent or null sends at once.
	SendAt *time.Time `json:"send_at"`
}

// The rules for a scheduled send time, in Istanbul time.
const (
	scheduleMinLead  = time.Minute
	scheduleMaxAhead = 14 * 24 * time.Hour
	quietHoursStart  = 21 // no send scheduled from 21:00...
	quietHoursEnd    = 9  // ...until 09:00
)

// checkSendAt reports why sendAt can't be scheduled, nil when it can: it must be at least
// a minute ahead, at most 14 days ahead, and outside the quiet hours (21:00–09:00).
// Immediate sends and retries aren't held to the quiet hours.
func checkSendAt(now, sendAt time.Time) []validation.FieldError {
	local := sendAt.In(istanbul)
	switch {
	case sendAt.Before(now.Add(scheduleMinLead)):
		return []validation.FieldError{{Path: "send_at", Code: "in_past",
			Message: "The send time must be at least a minute from now."}}
	case sendAt.After(now.Add(scheduleMaxAhead)):
		return []validation.FieldError{{Path: "send_at", Code: "too_far",
			Message: "The send time can be at most 14 days ahead."}}
	case local.Hour() >= quietHoursStart || local.Hour() < quietHoursEnd:
		return []validation.FieldError{{Path: "send_at", Code: "quiet_hours",
			Message: fmt.Sprintf("Messages can't be scheduled between %02d:00 and %02d:00 (Istanbul time).",
				quietHoursStart, quietHoursEnd)}}
	}
	return nil
}

type approveResponse struct {
	Campaign campaignView `json:"campaign"`
	Queued   store.Queued `json:"queued"`
}

// approve validates the final (possibly hand-edited) content of each needed language with
// the same rules as the model's draft and queues the messages, each recipient's in their
// language; with send_at they wait for that time (checkSendAt). A draft from the messages step is not required:
// the content is checked the same way whoever wrote it.
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	c, ok := s.campaign(w, r)
	if !ok {
		return
	}
	var req approveRequest
	if !decode(w, r, &req) {
		return
	}
	switch {
	case !editable(c):
		writeError(w, http.StatusConflict, "already_approved", "The campaign has already been approved.")
		return
	case c.Audience == nil:
		writeError(w, http.StatusConflict, "no_audience", "Build the audience before approving.")
		return
	case c.NeedsAcknowledgement():
		writeError(w, http.StatusConflict, "unsupported_not_acknowledged",
			"Parts of the brief aren't in the audience. Acknowledge them before approving.")
		return
	case segment.DaysAgo(s.now(), c.Audience.AsOf) != 0:
		// The day window ("last 30 days") was counted on an earlier day.
		writeError(w, http.StatusConflict, "stale_audience",
			"The audience was counted on an earlier day. Refresh it before approving.")
		return
	}
	if req.SendAt != nil {
		if errs := checkSendAt(s.now(), *req.SendAt); errs != nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid_send_at", "The send time isn't allowed.", errs...)
			return
		}
	}
	final, verr := content.ValidateSet(req.Contents, c.ContentOptions())
	if verr != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_content", "The messages don't pass validation.", fieldErrors(verr)...)
		return
	}
	seg, err := s.validator.Parse(c.Audience.Segment)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if !s.lock(c.ID) {
		writeBusy(w)
		return
	}
	defer s.unlock(c.ID)

	queued, err := s.store.Approve(r.Context(), c.ID, final, seg, c.Audience.AsOf, req.SendAt)
	switch {
	case errors.Is(err, store.ErrNotEditable):
		writeError(w, http.StatusConflict, "already_approved", "The campaign has already been approved.")
		return
	case errors.Is(err, store.ErrMissingLanguage):
		writeError(w, http.StatusConflict, "stale_audience",
			"Some recipients prefer a language the messages aren't written in. Refresh the audience and write the missing language.")
		return
	case errors.Is(err, store.ErrNoRecipients):
		writeError(w, http.StatusUnprocessableEntity, "empty_audience",
			"Nobody in this audience can be reached on any channel any more.")
		return
	case err != nil:
		internalError(w, r, err)
		return
	}
	updated, err := s.store.Get(r.Context(), c.ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	v, err := s.view(updated)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, approveResponse{Campaign: v, Queued: queued})
}

// cancelSchedule serves POST /api/campaigns/{id}/schedule/cancel: a scheduled campaign
// whose send hasn't started goes back to ready, its queued messages deleted.
func (s *Server) cancelSchedule(w http.ResponseWriter, r *http.Request) {
	c, ok := s.campaign(w, r)
	if !ok {
		return
	}
	if !s.lock(c.ID) {
		writeBusy(w)
		return
	}
	defer s.unlock(c.ID)

	err := s.store.CancelSchedule(r.Context(), c.ID)
	switch {
	case errors.Is(err, store.ErrNotScheduled):
		writeError(w, http.StatusConflict, "not_scheduled", "The campaign isn't scheduled.")
		return
	case errors.Is(err, store.ErrSendStarted):
		writeError(w, http.StatusConflict, "send_started", "The scheduled send has already started.")
		return
	case err != nil:
		internalError(w, r, err)
		return
	}
	slog.WarnContext(r.Context(), "scheduled send canceled", "campaign_id", c.ID, "scheduled_at", c.ScheduledAt)
	s.getCampaign(w, r)
}
