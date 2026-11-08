// Package llm is the boundary to the language model. Client is deliberately thin: it sends
// a request and returns the model's raw structured output. Parsing, validation, repair and
// retries live in the caller, so they run identically against Claude and the mock.
package llm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Step string

const (
	StepAudience Step = "audience"
	StepMessages Step = "messages"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one conversation turn. In a repair round, an assistant turn carries the
// model's previous raw output and the following user turn carries the validation errors;
// the Claude client maps that pair to a tool_use / tool_result (is_error) exchange.
type Message struct {
	Role    Role
	Content string
}

// Tool is the structured-output contract for a step: the model must answer by "calling"
// it, so its answer is JSON matching InputSchema rather than free text.
type Tool struct {
	Name        string
	Description string
	InputSchema []byte // JSON Schema
}

type Request struct {
	Step     Step
	System   string
	Messages []Message
	Tool     Tool
	// OnDelta, if set, receives the tool-input JSON as it is generated, in order. Chunks
	// are partial JSON (not valid on their own) and always split on rune boundaries.
	OnDelta func(chunk string)
}

type Response struct {
	// Raw is the tool input exactly as the model produced it: unparsed and unvalidated.
	// It may be malformed JSON; that's the caller's validation's job to catch.
	Raw          []byte
	InputTokens  int
	OutputTokens int
}

// Client is implemented by the Claude client and by Mock.
type Client interface {
	Generate(ctx context.Context, req Request) (Response, error)
}

// TransportError means no usable response arrived (network error, timeout, 429, 5xx...),
// as opposed to a response that arrived but failed validation. The two are retried on
// separate budgets.
type TransportError struct {
	// StatusCode is the HTTP status, or 0 for network errors and timeouts.
	StatusCode int
	// RetryAfter is the server's retry-after hint, 0 if none.
	RetryAfter time.Duration
	// Partial is set when the stream broke after output had started, i.e. OnDelta has
	// already been called. A retry can't be silent then: the UI must discard the draft.
	Partial bool
	Err     error
}

func (e *TransportError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("llm transport: status %d: %v", e.StatusCode, e.Err)
	}
	return fmt.Sprintf("llm transport: %v", e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// Retryable reports whether the same request may succeed if sent again. 400/401/403 and
// other 4xx are bugs or config errors, not transient.
func (e *TransportError) Retryable() bool {
	switch {
	case e.StatusCode == 0, e.StatusCode == 429, e.StatusCode >= 500:
		return true
	default:
		return false
	}
}

// IsRetryable reports whether err is a retryable TransportError.
func IsRetryable(err error) bool {
	var te *TransportError
	return errors.As(err, &te) && te.Retryable()
}
