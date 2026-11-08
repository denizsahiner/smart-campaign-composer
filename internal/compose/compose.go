// Package compose runs the LLM steps of a campaign: call the model, validate the answer,
// repair it or retry the call, and report progress as events for the SSE stream.
//
// Two budgets, per step, that never consume each other:
//   - repairs: the answer arrived but failed validation; the model is shown its answer and
//     the errors and asked to fix them.
//   - transport retries: no usable answer arrived (network error, timeout, 429, 5xx);
//     the same request is sent again after a backoff.
//
// With the defaults a step makes at most 1 + 2 + 2 = 5 calls. The marketer is waiting on
// screen, so the budgets are small and a long retry-after fails fast instead of waiting.
package compose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/prompt"
	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

type Policy struct {
	MaxRepairs          int
	MaxTransportRetries int
	// BaseBackoff doubles per transport retry (1s, 2s, ...), with jitter.
	BaseBackoff time.Duration
	// MaxRetryAfter: if the server asks to wait longer than this, give up immediately
	// ("Claude is busy, try again") rather than keep the marketer waiting.
	MaxRetryAfter time.Duration
}

func DefaultPolicy() Policy {
	return Policy{MaxRepairs: 2, MaxTransportRetries: 2, BaseBackoff: time.Second, MaxRetryAfter: 10 * time.Second}
}

// ErrUnavailable means the model couldn't be reached within the transport budget.
var ErrUnavailable = errors.New("compose: Claude is busy or unreachable, try again shortly")

// ExhaustedError means every repair attempt still failed validation. Errors are the last
// attempt's.
type ExhaustedError struct {
	Step   llm.Step
	Errors []validation.FieldError
}

func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("compose: no valid %s answer after all repair attempts: %v",
		e.Step, (&validation.Error{Errors: e.Errors}).Error())
}

// Call is one model call, as stored in llm_calls.
type Call struct {
	CampaignID       int64
	Step             llm.Step
	PromptVersion    string
	Raw              []byte                  // nil when no answer arrived
	ValidationErrors []validation.FieldError // nil when the answer was valid
	Error            string                  // transport error or refusal, "" otherwise
	RepairAttempt    int                     // 0 for the first answer, 1.. for repairs
	TransportAttempt int                     // transport retries so far in this step
	InputTokens      int
	OutputTokens     int
	Model            string // the model that answered; "" when none did
	Latency          time.Duration
}

// Recorder persists calls. A recording failure is logged, never fails the step.
type Recorder interface {
	RecordCall(ctx context.Context, c Call) error
}

type Composer struct {
	llm      llm.Client
	recorder Recorder
	policy   Policy

	// Replaceable in tests.
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(d time.Duration) time.Duration
	now    func() time.Time
}

// New returns a Composer. recorder may be nil.
func New(client llm.Client, recorder Recorder, policy Policy) *Composer {
	return &Composer{
		llm: client, recorder: recorder, policy: policy,
		sleep: sleepCtx, jitter: equalJitter, now: time.Now,
	}
}

// run executes one step. parse validates an answer: a *validation.Error is repairable,
// any other error ends the step with that error (e.g. prompt.ErrUnmappable).
//
// It returns the last raw answer and, if repairs ran out, its validation error; err is
// set when there's no usable answer at all.
func (c *Composer) run(ctx context.Context, campaignID int64, original llm.Request,
	emit func(Event), parse func(raw []byte) error) (raw []byte, exhausted *validation.Error, err error) {

	req := original
	repairs, transportRetries := 0, 0
	for {
		started := false
		if original.Step == llm.StepMessages {
			req.OnDelta = func(chunk string) {
				started = true
				emit(Event{Type: EventDelta, Delta: chunk})
			}
		}

		t0 := c.now()
		res, genErr := c.llm.Generate(ctx, req)
		call := Call{
			CampaignID: campaignID, Step: original.Step, PromptVersion: prompt.Version,
			RepairAttempt: repairs, TransportAttempt: transportRetries, Latency: c.now().Sub(t0),
		}

		if genErr != nil {
			call.Error = genErr.Error()
			c.record(ctx, call)
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			var te *llm.TransportError
			if !errors.As(genErr, &te) || !te.Retryable() {
				return nil, nil, genErr // refusal, 4xx, misconfiguration: retrying won't help
			}
			if transportRetries >= c.policy.MaxTransportRetries || te.RetryAfter > c.policy.MaxRetryAfter {
				return nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, genErr)
			}
			transportRetries++
			wait := max(c.backoff(transportRetries), te.RetryAfter)
			// Silent before the first chunk; after it the UI has a half-drawn draft to clear.
			if te.Partial || started {
				emit(Event{Type: EventRetry, Reason: RetryInterrupted})
			}
			if err := c.sleep(ctx, wait); err != nil {
				return nil, nil, err
			}
			continue
		}

		call.Raw, call.InputTokens, call.OutputTokens, call.Model = res.Raw, res.InputTokens, res.OutputTokens, res.Model
		perr := parse(res.Raw)
		var verr *validation.Error
		if errors.As(perr, &verr) {
			call.ValidationErrors = verr.Errors
		}
		c.record(ctx, call)

		switch {
		case perr == nil:
			return res.Raw, nil, nil
		case verr == nil:
			return res.Raw, nil, perr
		case repairs >= c.policy.MaxRepairs:
			return res.Raw, verr, nil
		}
		repairs++
		emit(Event{Type: EventRetry, Reason: RetryInvalid, Errors: verr.Errors})
		// Each round starts from the original request (see prompt.Repair).
		req = prompt.Repair(original, res.Raw, verr)
	}
}

func (c *Composer) record(ctx context.Context, call Call) {
	if c.recorder == nil {
		return
	}
	// The log row matters most when the request was cancelled or failed, so don't let
	// the request's cancellation drop it.
	if err := c.recorder.RecordCall(context.WithoutCancel(ctx), call); err != nil {
		slog.Warn("record llm call", "campaign_id", call.CampaignID, "step", call.Step, "err", err)
	}
}

// backoff returns the jittered wait before transport retry n (1-based): 1s, 2s, 4s, ...
func (c *Composer) backoff(n int) time.Duration {
	return c.jitter(c.policy.BaseBackoff << (n - 1))
}

// equalJitter waits between d/2 and d, so concurrent retries spread out but never come
// back much sooner than intended.
func equalJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
