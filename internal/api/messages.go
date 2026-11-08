package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/denizsahiner/smart-campaign-composer/internal/compose"
	"github.com/denizsahiner/smart-campaign-composer/internal/prompt"
)

type messagesRequest struct {
	// Feedback asks for a new version of the stored draft ("more formal").
	Feedback string `json:"feedback"`
}

// generateMessages runs the messages step and streams it as Server-Sent Events: delta,
// retry, then exactly one of validated, invalid or error (see compose.Event).
//
// It is a POST, not a GET, because it creates content; the frontend reads the stream
// with fetch rather than EventSource. Preconditions are checked before the stream starts
// and fail as ordinary JSON errors. If the client disconnects, the model call is
// cancelled and nothing is stored.
func (s *Server) generateMessages(w http.ResponseWriter, r *http.Request) {
	c, ok := s.campaign(w, r)
	if !ok {
		return
	}
	var req messagesRequest
	if !decode(w, r, &req) {
		return
	}
	req.Feedback = strings.TrimSpace(req.Feedback)

	switch {
	case !editable(c):
		writeNotEditable(w)
		return
	case c.Audience == nil:
		writeError(w, http.StatusConflict, "no_audience", "Build the audience before generating messages.")
		return
	case c.NeedsAcknowledgement():
		writeError(w, http.StatusConflict, "unsupported_not_acknowledged",
			"Parts of the brief aren't in the audience. Acknowledge them before generating messages.")
		return
	case c.Audience.Counts.SMS+c.Audience.Counts.Email == 0:
		writeError(w, http.StatusUnprocessableEntity, "empty_audience",
			"Nobody in this audience can be reached on any channel. Change the brief.")
		return
	case utf8.RuneCountInString(req.Feedback) > maxFeedbackRunes:
		writeError(w, http.StatusUnprocessableEntity, "invalid_feedback",
			fmt.Sprintf("Feedback must be at most %d characters.", maxFeedbackRunes))
		return
	case req.Feedback != "" && c.Content == nil:
		writeError(w, http.StatusUnprocessableEntity, "no_draft", "There is no draft to revise yet.")
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

	in := prompt.MessagesInput{Brief: c.Brief, Segment: seg, LongestFirstName: c.Audience.Counts.LongestFirstName,
		Language: prompt.DefaultLanguage}
	if req.Feedback != "" {
		in.Previous, in.Feedback = c.Content, req.Feedback
	}

	sse := newSSE(w)
	// The final event is held back until the draft is stored, so a client that reloads
	// the campaign on "validated" or "invalid" finds the content there.
	var final *compose.Event
	res, err := s.composer.Messages(r.Context(), c.ID, in, func(e compose.Event) {
		if e.Type == compose.EventValidated || e.Type == compose.EventInvalid {
			final = &e
			return
		}
		sse.send(e)
	})
	if err != nil || final == nil {
		return // cancelled, or an error event has been sent
	}
	if err := s.store.SetContent(r.Context(), c.ID, res.Content, in.Language, res.Errors); err != nil {
		slog.Error("store draft", "campaign_id", c.ID, "err", err)
		sse.send(compose.Event{Type: compose.EventError, Message: "The draft couldn't be saved. Please try again."})
		return
	}
	sse.send(*final)
}

type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSE(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	w.WriteHeader(http.StatusOK)
	s := &sseWriter{w: w, rc: http.NewResponseController(w)}
	s.rc.Flush()
	return s
}

// send writes one event; the event name is the type, the data its JSON.
func (s *sseWriter) send(e compose.Event) {
	data, err := json.Marshal(e)
	if err != nil {
		data, _ = json.Marshal(compose.Event{Type: compose.EventError, Message: "internal error"})
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", e.Type, data)
	s.rc.Flush()
}
