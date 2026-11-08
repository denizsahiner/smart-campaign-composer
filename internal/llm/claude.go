package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// DefaultModel is used when ANTHROPIC_MODEL is unset. Sonnet 5.5 matched Opus 5.5 on
// accuracy for both steps in our comparison and was 2–3x faster on the messages step at
// about a third of the cost (DECISIONS.md).
const DefaultModel = "claude-sonnet-5-5"

// ErrTruncated means the answer hit max_tokens (or the context window) and is incomplete.
// Repairing or resending the same request won't make it fit.
var ErrTruncated = errors.New("llm: answer truncated at the output token limit")

var errFirstEventTimeout = errors.New("llm: no response within the first-event timeout")
var errIdleTimeout = errors.New("llm: stream stalled longer than the idle timeout")

type ClaudeConfig struct {
	APIKey string
	Model  string
	// Effort per step ("low" … "max"). Higher effort thinks longer: more latency and
	// tokens. The marketer waits on screen, so the defaults are low.
	Effort    map[Step]string
	MaxTokens int64
	// FirstEventTimeout bounds the wait for the stream to start; IdleTimeout the gap
	// between events once it has. A single total timeout would either kill long but
	// healthy generations or wait far too long on a dead connection.
	FirstEventTimeout time.Duration
	IdleTimeout       time.Duration
	// RefusalFallback lets the API re-run a request on another model server-side when the
	// model declines it (fallbacks: "default"). Not supported on Haiku; skipped there.
	RefusalFallback bool
	// BaseURL overrides the API endpoint (tests).
	BaseURL string
}

func DefaultClaudeConfig() ClaudeConfig {
	return ClaudeConfig{
		Model:             DefaultModel,
		Effort:            map[Step]string{StepAudience: "low", StepMessages: "medium"},
		MaxTokens:         16000,
		FirstEventTimeout: 15 * time.Second,
		IdleTimeout:       20 * time.Second,
		RefusalFallback:   true,
	}
}

// Claude is the Client backed by the Claude API.
type Claude struct {
	client anthropic.Client
	cfg    ClaudeConfig
}

func NewClaude(cfg ClaudeConfig) *Claude {
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		// Retries are compose's job (separate repair and transport budgets). SDK retries
		// on top would multiply attempts and hide them from llm_calls.
		option.WithMaxRetries(0),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &Claude{client: anthropic.NewClient(opts...), cfg: cfg}
}

func (c *Claude) Generate(ctx context.Context, req Request) (Response, error) {
	params, err := c.params(req)
	if err != nil {
		return Response{}, err
	}
	var opts []option.RequestOption
	if c.cfg.RefusalFallback && !strings.Contains(c.cfg.Model, "haiku") {
		opts = append(opts,
			option.WithHeaderAdd("anthropic-beta", "server-side-fallback-2026-07-01"),
			option.WithJSONSet("fallbacks", "default"))
	}

	// The watchdog cancels the stream when it doesn't start, or stalls, in time.
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(c.cfg.FirstEventTimeout, func() { cancel(errFirstEventTimeout) })
	defer watchdog.Stop()

	stream := c.client.Messages.NewStreaming(sctx, params, opts...)
	defer stream.Close()

	var (
		res        Response
		raw        strings.Builder
		started    bool
		stopReason anthropic.StopReason
		stop       anthropic.RefusalStopDetails
	)
	for stream.Next() {
		watchdog.Stop()
		watchdog = time.AfterFunc(c.cfg.IdleTimeout, func() { cancel(errIdleTimeout) })

		switch ev := stream.Current().AsAny().(type) {
		case anthropic.MessageStartEvent:
			res.Model = string(ev.Message.Model)
			res.InputTokens = int(ev.Message.Usage.InputTokens)
		case anthropic.ContentBlockDeltaEvent:
			// Only text is the answer. Thinking deltas are skipped; after a server-side
			// fallback the text continues in a new block, so all text is concatenated.
			if d, ok := ev.Delta.AsAny().(anthropic.TextDelta); ok && d.Text != "" {
				started = true
				raw.WriteString(d.Text)
				if req.OnDelta != nil {
					req.OnDelta(d.Text)
				}
			}
		case anthropic.MessageDeltaEvent:
			stopReason = ev.Delta.StopReason
			stop = ev.Delta.StopDetails
			res.OutputTokens = int(ev.Usage.OutputTokens)
		}
	}
	if err := stream.Err(); err != nil {
		return Response{}, c.classify(ctx, sctx, err, started)
	}

	switch stopReason {
	case anthropic.StopReasonRefusal:
		return Response{}, &RefusalError{Category: string(stop.Category), Explanation: stop.Explanation}
	case anthropic.StopReasonMaxTokens, anthropic.StopReasonModelContextWindowExceeded:
		return Response{}, ErrTruncated
	}
	res.Raw = []byte(raw.String())
	return res, nil
}

func (c *Claude) params(req Request) (anthropic.MessageNewParams, error) {
	var schema map[string]any
	if err := json.Unmarshal(req.Schema, &schema); err != nil {
		return anthropic.MessageNewParams{}, fmt.Errorf("llm: invalid output schema: %w", err)
	}
	msgs := make([]anthropic.MessageParam, 0, len(req.Messages))
	for _, m := range req.Messages {
		block := anthropic.NewTextBlock(m.Content)
		if m.Role == RoleAssistant {
			msgs = append(msgs, anthropic.NewAssistantMessage(block))
		} else {
			msgs = append(msgs, anthropic.NewUserMessage(block))
		}
	}
	return anthropic.MessageNewParams{
		Model:     anthropic.Model(c.cfg.Model),
		MaxTokens: c.cfg.MaxTokens,
		System:    []anthropic.TextBlockParam{{Text: req.System}},
		Messages:  msgs,
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(c.cfg.Effort[req.Step]),
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
	}, nil
}

// classify maps a stream error onto the errors compose understands.
func (c *Claude) classify(parent, sctx context.Context, err error, started bool) error {
	if parent.Err() != nil {
		return parent.Err() // the caller gave up; not a transport failure
	}
	if cause := context.Cause(sctx); errors.Is(cause, errFirstEventTimeout) || errors.Is(cause, errIdleTimeout) {
		return &TransportError{Partial: started, Err: cause}
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		// An error event inside a 200 stream (e.g. overloaded mid-generation) carries
		// its kind only in the error type.
		if status == 0 || status == http.StatusOK {
			status = statusForErrorType(string(apiErr.Type()))
		}
		te := &TransportError{StatusCode: status, Partial: started, Err: err}
		if apiErr.Response != nil {
			te.RetryAfter = retryAfter(apiErr.Response.Header)
		}
		return te
	}
	return &TransportError{Partial: started, Err: err} // network error
}

func statusForErrorType(t string) int {
	switch t {
	case "invalid_request_error":
		return 400
	case "authentication_error":
		return 401
	case "billing_error":
		return 402
	case "permission_error":
		return 403
	case "not_found_error":
		return 404
	case "request_too_large":
		return 413
	case "rate_limit_error":
		return 429
	case "overloaded_error":
		return 529
	case "timeout_error":
		return 504
	default: // api_error and anything new: treat as a server error
		return 500
	}
}

// retryAfter parses Retry-After as seconds or an HTTP date; 0 if absent or invalid.
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}
