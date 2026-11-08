package compose

import (
	"context"
	"errors"

	"github.com/denizsahiner/smart-campaign-composer/internal/content"
	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/prompt"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

type EventType string

// Event types map one-to-one onto SSE event names.
const (
	// EventDelta carries a chunk of the draft as it's generated; unvalidated.
	EventDelta EventType = "delta"
	// EventRetry: the draft so far is discarded and generation restarts.
	EventRetry EventType = "retry"
	// EventValidated carries the final, valid content. Approval is possible from here.
	EventValidated EventType = "validated"
	// EventInvalid: repairs ran out. Content is the last draft, editable, and Errors say
	// what to fix; approval stays blocked until it passes validation.
	EventInvalid EventType = "invalid"
	// EventError: no usable answer (model unavailable, refusal). Message is for the UI.
	EventError EventType = "error"
)

type RetryReason string

const (
	RetryInvalid     RetryReason = "invalid"     // the answer failed validation
	RetryInterrupted RetryReason = "interrupted" // the stream broke mid-way
)

type Event struct {
	Type    EventType               `json:"type"`
	Delta   string                  `json:"delta,omitempty"`
	Reason  RetryReason             `json:"reason,omitempty"`
	Errors  []validation.FieldError `json:"errors,omitempty"`
	Content *content.Content        `json:"content,omitempty"`
	Message string                  `json:"message,omitempty"`
}

// Audience runs the audience step. Besides infrastructure errors it returns
// prompt.ErrUnmappable (nothing in the brief is expressible; Unsupported says why) or an
// *ExhaustedError (no valid segment after the repairs); for both the marketer has to
// change the brief. Unsupported is filled whenever the model answered.
func (c *Composer) Audience(ctx context.Context, campaignID int64, brief string, v *segment.Validator) (prompt.Audience, error) {
	var a prompt.Audience
	_, exhausted, err := c.run(ctx, campaignID, prompt.AudienceRequest(brief, v.Cities()), func(Event) {},
		func(raw []byte) error {
			var perr error
			a, perr = prompt.ParseAudience(raw, v)
			return perr
		})
	if err != nil {
		return a, err
	}
	if exhausted != nil {
		return a, &ExhaustedError{Step: llm.StepAudience, Errors: exhausted.Errors}
	}
	return a, nil
}

// MessagesResult is the outcome of the messages step. When Valid is false, Content is the
// last draft for the marketer to fix and Errors say what's wrong.
type MessagesResult struct {
	Content content.Content
	Valid   bool
	Errors  []validation.FieldError
}

// Messages runs the messages step, streaming progress to emit (which may be nil). The
// last event is always validated, invalid or error. An error is returned only when there
// is no usable draft at all.
func (c *Composer) Messages(ctx context.Context, campaignID int64, in prompt.MessagesInput, emit func(Event)) (MessagesResult, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	var parsed content.Content
	raw, exhausted, err := c.run(ctx, campaignID, prompt.MessagesRequest(in), emit,
		func(raw []byte) error {
			var perr error
			parsed, perr = prompt.ParseMessages(raw, in.LongestFirstName)
			return perr
		})
	if err != nil {
		if ctx.Err() == nil {
			emit(Event{Type: EventError, Message: errorMessage(err)})
		}
		return MessagesResult{}, err
	}
	if exhausted == nil {
		emit(Event{Type: EventValidated, Content: &parsed})
		return MessagesResult{Content: parsed, Valid: true}, nil
	}

	draft, derr := content.Decode(raw)
	if derr != nil {
		// Not even decodable (structured outputs make this very unlikely): nothing to edit.
		err := &ExhaustedError{Step: llm.StepMessages, Errors: exhausted.Errors}
		emit(Event{Type: EventError, Message: errorMessage(err), Errors: exhausted.Errors})
		return MessagesResult{}, err
	}
	emit(Event{Type: EventInvalid, Content: &draft, Errors: exhausted.Errors})
	return MessagesResult{Content: draft, Errors: exhausted.Errors}, nil
}

// errorMessage turns a step error into text for the marketer; details go to the logs and
// llm_calls, not the UI.
func errorMessage(err error) string {
	var refusal *llm.RefusalError
	switch {
	case errors.Is(err, ErrUnavailable):
		return "Claude is busy or unreachable right now. Please try again in a moment."
	case errors.As(err, &refusal):
		return "Claude declined to write this campaign. Please rephrase the brief."
	default:
		return "Message generation failed. Please try again."
	}
}
