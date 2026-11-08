package api

import (
	"errors"
	"net/http"

	"github.com/denizsahiner/smart-campaign-composer/internal/content"
	"github.com/denizsahiner/smart-campaign-composer/internal/prompt"
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
// how a stale audience (counted on an earlier day) becomes approvable again. The draft
// and the acknowledgement are kept; the draft is re-validated in case the longest name
// in the audience grew.
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
	if c.Content != nil {
		if _, verr := content.Validate(*c.Content, content.Options{LongestFirstName: counts.LongestFirstName}); verr != nil {
			contentErrs = fieldErrors(verr)
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
	Content content.Content `json:"content"`
}

type approveResponse struct {
	Campaign campaignView `json:"campaign"`
	Queued   store.Queued `json:"queued"`
}

// approve validates the final (possibly hand-edited) content with the same rules as the
// model's draft and queues the messages. A draft from the messages step is not required:
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
	final, verr := content.Validate(req.Content, content.Options{LongestFirstName: c.Audience.Counts.LongestFirstName})
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

	lang := c.ContentLanguage
	if lang == "" {
		lang = prompt.DefaultLanguage
	}
	queued, err := s.store.Approve(r.Context(), c.ID, final, lang, seg, c.Audience.AsOf)
	switch {
	case errors.Is(err, store.ErrNotEditable):
		writeError(w, http.StatusConflict, "already_approved", "The campaign has already been approved.")
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
