package compose

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denizsahiner/smart-campaign-composer/internal/content"
	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/prompt"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

const (
	validMessages = `{"sms":"Merhaba {{first_name}}, sepetin seni bekliyor!","email_subject":"Sepetin %30 indirimli","email_body":"Merhaba {{first_name}},\n\nSepetin seni bekliyor."}`
	validAudience = `{"rules":[{"field":"city","op":"eq","value":"istanbul"}],"unsupported_criteria":[]}`
	campaignID    = 7
)

var validator = segment.NewValidator([]string{"Ankara", "İstanbul"})

// 24 + 8×19 = 176 characters rendered with a 15-character name.
var tooLongMessages = `{"sms":"Merhaba {{first_name}}, ` + strings.Repeat("çok uzun mesaj var ", 8) + `","email_subject":"Konu","email_body":"Gövde"}`

type recorder struct{ calls []Call }

func (r *recorder) RecordCall(_ context.Context, c Call) error {
	r.calls = append(r.calls, c)
	return nil
}

type harness struct {
	c      *Composer
	mock   *llm.Mock
	rec    *recorder
	sleeps []time.Duration
	events []Event
}

func newHarness(replies ...llm.Scripted) *harness {
	h := &harness{mock: llm.NewScripted(replies...), rec: &recorder{}}
	h.c = New(h.mock, h.rec, DefaultPolicy())
	h.c.jitter = func(d time.Duration) time.Duration { return d }
	h.c.sleep = func(_ context.Context, d time.Duration) error {
		h.sleeps = append(h.sleeps, d)
		return nil
	}
	return h
}

func (h *harness) emit(e Event) { h.events = append(h.events, e) }

// eventTypes returns event types without deltas, to compare sequences.
func (h *harness) eventTypes() []string {
	var out []string
	for _, e := range h.events {
		if e.Type != EventDelta {
			s := string(e.Type)
			if e.Reason != "" {
				s += ":" + string(e.Reason)
			}
			out = append(out, s)
		}
	}
	return out
}

func messagesInput(t *testing.T) prompt.MessagesInput {
	t.Helper()
	a, err := prompt.ParseAudience([]byte(validAudience), validator)
	if err != nil {
		t.Fatal(err)
	}
	return prompt.MessagesInput{Brief: "Sepeti terk edenlere %30 indirim.", Segment: a.Segment, LongestFirstName: 15}
}

func busy(status int, retryAfter time.Duration) llm.Scripted {
	return llm.Scripted{Err: &llm.TransportError{StatusCode: status, RetryAfter: retryAfter, Err: errors.New("busy")}}
}

func TestMessagesHappyPath(t *testing.T) {
	h := newHarness(llm.Scripted{Raw: validMessages})
	res, err := h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit)
	if err != nil || !res.Valid || res.Content.SMS != "Merhaba {{first_name}}, sepetin seni bekliyor!" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	var streamed strings.Builder
	for _, e := range h.events {
		if e.Type == EventDelta {
			streamed.WriteString(e.Delta)
		}
	}
	if streamed.String() != validMessages {
		t.Errorf("deltas = %q", streamed.String())
	}
	if got := h.eventTypes(); !slices.Equal(got, []string{"validated"}) {
		t.Errorf("events = %v", got)
	}
	if c := h.rec.calls; len(c) != 1 || c[0].ValidationErrors != nil || c[0].PromptVersion != prompt.Version ||
		c[0].CampaignID != campaignID || c[0].Step != llm.StepMessages || string(c[0].Raw) != validMessages {
		t.Errorf("recorded = %+v", c)
	}
}

// Malformed JSON, then a too-long SMS, then a valid answer: two repair rounds.
func TestMessagesRepairs(t *testing.T) {
	h := newHarness(
		llm.Scripted{Raw: `{"sms":"Merhaba`},
		llm.Scripted{Raw: tooLongMessages},
		llm.Scripted{Raw: validMessages},
	)
	res, err := h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit)
	if err != nil || !res.Valid {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if got := h.eventTypes(); !slices.Equal(got, []string{"retry:invalid", "retry:invalid", "validated"}) {
		t.Errorf("events = %v", got)
	}
	if len(h.sleeps) != 0 {
		t.Errorf("repairs must not back off: %v", h.sleeps)
	}

	// Each repair quotes the previous answer and its errors, and starts from the original
	// request: the second repair doesn't carry the first one's text.
	calls := h.mock.Calls()
	second, third := calls[1].Messages[0].Content, calls[2].Messages[0].Content
	if !strings.Contains(second, "<previous_answer>\n{\"sms\":\"Merhaba\n</previous_answer>") || !strings.Contains(second, "- $: ") {
		t.Errorf("first repair doesn't quote the broken answer:\n%s", second)
	}
	if !strings.Contains(third, "sms: is ") || !strings.Contains(third, "shorten by at least") || strings.Count(third, "<previous_answer>") != 1 {
		t.Errorf("second repair:\n%s", third)
	}

	rec := h.rec.calls
	if len(rec) != 3 || rec[0].RepairAttempt != 0 || rec[2].RepairAttempt != 2 ||
		rec[0].ValidationErrors[0].Code != validation.CodeInvalidJSON || rec[1].ValidationErrors[0].Code != content.CodeTooLong || rec[2].ValidationErrors != nil {
		t.Errorf("recorded = %+v", rec)
	}
}

// Repairs run out: the last draft is handed to the marketer, not an error.
func TestMessagesExhaustedGivesEditableDraft(t *testing.T) {
	h := newHarness(llm.Scripted{Raw: tooLongMessages}, llm.Scripted{Raw: tooLongMessages}, llm.Scripted{Raw: tooLongMessages})
	res, err := h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit)
	if err != nil {
		t.Fatalf("err = %v, want an editable draft", err)
	}
	if res.Valid || res.Content.EmailSubject != "Konu" || len(res.Errors) != 1 || res.Errors[0].Code != content.CodeTooLong {
		t.Fatalf("res = %+v", res)
	}
	if got := h.eventTypes(); !slices.Equal(got, []string{"retry:invalid", "retry:invalid", "invalid"}) {
		t.Errorf("events = %v", got)
	}
	last := h.events[len(h.events)-1]
	if last.Content == nil || last.Content.EmailBody != "Gövde" || len(last.Errors) != 1 {
		t.Errorf("invalid event = %+v", last)
	}
	if h.mock.Remaining() != 0 || len(h.mock.Calls()) != 3 {
		t.Errorf("calls = %d, want 3 (1 + 2 repairs)", len(h.mock.Calls()))
	}
}

// Transport retries are silent before output starts, back off exponentially, and don't
// consume repairs.
func TestTransportRetries(t *testing.T) {
	h := newHarness(busy(529, 0), busy(500, 0), llm.Scripted{Raw: validMessages})
	res, err := h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit)
	if err != nil || !res.Valid {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if got := h.eventTypes(); !slices.Equal(got, []string{"validated"}) {
		t.Errorf("events = %v (retries before the first token must be silent)", got)
	}
	if !slices.Equal(h.sleeps, []time.Duration{time.Second, 2 * time.Second}) {
		t.Errorf("sleeps = %v", h.sleeps)
	}
	rec := h.rec.calls
	if len(rec) != 3 || rec[0].Error == "" || rec[0].Raw != nil || rec[1].TransportAttempt != 1 || rec[2].TransportAttempt != 2 || rec[2].RepairAttempt != 0 {
		t.Errorf("recorded = %+v", rec)
	}
}

func TestTransportBudgetExhausted(t *testing.T) {
	h := newHarness(busy(529, 0), busy(529, 0), busy(529, 0), llm.Scripted{Raw: validMessages})
	_, err := h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if got := h.eventTypes(); !slices.Equal(got, []string{"error"}) {
		t.Errorf("events = %v", got)
	}
	if h.mock.Remaining() != 1 {
		t.Errorf("made %d calls, want 3", 4-h.mock.Remaining())
	}
}

// Both budgets are spent independently in one step: 2 repairs and 2 transport retries.
func TestBudgetsAreIndependent(t *testing.T) {
	h := newHarness(
		llm.Scripted{Raw: tooLongMessages},
		busy(429, 0),
		llm.Scripted{Raw: tooLongMessages},
		busy(503, 0),
		llm.Scripted{Raw: validMessages},
	)
	res, err := h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit)
	if err != nil || !res.Valid {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if h.mock.Remaining() != 0 {
		t.Error("not all replies used")
	}
	// The transport retry after a repair resends the repair request, not the original.
	calls := h.mock.Calls()
	if calls[2].Messages[0].Content != calls[1].Messages[0].Content || !strings.Contains(calls[1].Messages[0].Content, "<previous_answer>") {
		t.Error("transport retry didn't resend the same repair request")
	}
}

func TestRetryAfter(t *testing.T) {
	// A retry-after longer than the backoff is respected.
	h := newHarness(busy(429, 5*time.Second), llm.Scripted{Raw: validMessages})
	if _, err := h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.sleeps, []time.Duration{5 * time.Second}) {
		t.Errorf("sleeps = %v, want [5s]", h.sleeps)
	}

	// Too long to keep the marketer waiting: fail at once, don't sleep.
	h = newHarness(busy(429, 30*time.Second), llm.Scripted{Raw: validMessages})
	if _, err := h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if len(h.sleeps) != 0 {
		t.Errorf("slept %v", h.sleeps)
	}
}

func TestNonRetryableErrors(t *testing.T) {
	cases := map[string]error{
		"bad request": &llm.TransportError{StatusCode: 400, Err: errors.New("bad")},
		"auth":        &llm.TransportError{StatusCode: 401, Err: errors.New("key")},
		"refusal":     &llm.RefusalError{Category: "cyber"},
		"unknown":     errors.New("boom"),
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(llm.Scripted{Err: e}, llm.Scripted{Raw: validMessages})
			_, err := h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit)
			if !errors.Is(err, e) {
				t.Fatalf("err = %v, want %v", err, e)
			}
			if h.mock.Remaining() != 1 || len(h.sleeps) != 0 {
				t.Errorf("retried a non-retryable error")
			}
			if got := h.eventTypes(); !slices.Equal(got, []string{"error"}) {
				t.Errorf("events = %v", got)
			}
		})
	}
	h := newHarness(llm.Scripted{Err: &llm.RefusalError{}})
	h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit)
	if msg := h.events[len(h.events)-1].Message; !strings.Contains(msg, "declined") {
		t.Errorf("refusal message = %q", msg)
	}
}

// A stream that breaks after output started: the UI is told to discard the draft.
func TestInterruptedStream(t *testing.T) {
	h := newHarness(
		llm.Scripted{Raw: `{"sms":"Merhaba {{first`, Err: &llm.TransportError{Partial: true, Err: errors.New("reset")}},
		llm.Scripted{Raw: validMessages},
	)
	res, err := h.c.Messages(t.Context(), campaignID, messagesInput(t), h.emit)
	if err != nil || !res.Valid {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if got := h.eventTypes(); !slices.Equal(got, []string{"retry:interrupted", "validated"}) {
		t.Errorf("events = %v", got)
	}
	// Deltas of the broken stream come before the retry event, the new stream's after it.
	i := slices.IndexFunc(h.events, func(e Event) bool { return e.Type == EventRetry })
	if i == 0 || h.events[i-1].Type != EventDelta || h.events[i+1].Type != EventDelta {
		t.Errorf("events around retry: %+v", h.events)
	}
}

func TestCancelDuringBackoff(t *testing.T) {
	h := newHarness(busy(529, 0), llm.Scripted{Raw: validMessages})
	ctx, cancel := context.WithCancel(t.Context())
	h.c.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return sleepCtx(ctx, d)
	}
	_, err := h.c.Messages(ctx, campaignID, messagesInput(t), h.emit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(h.events) != 0 {
		t.Errorf("events after the client went away: %v", h.eventTypes())
	}
	if len(h.rec.calls) != 1 {
		t.Errorf("the failed call must still be recorded")
	}
}

func TestGuidedRegeneration(t *testing.T) {
	h := newHarness(llm.Scripted{Raw: validMessages})
	in := messagesInput(t)
	in.Previous = &content.Content{SMS: "Eski SMS", EmailSubject: "Eski konu", EmailBody: "Eski gövde"}
	in.Feedback = "Daha resmi olsun"
	if _, err := h.c.Messages(t.Context(), campaignID, in, h.emit); err != nil {
		t.Fatal(err)
	}
	sent := h.mock.Calls()[0].Messages[0].Content
	if !strings.Contains(sent, "Eski SMS") || !strings.Contains(sent, "Daha resmi olsun") {
		t.Errorf("regeneration request missing draft or feedback:\n%s", sent)
	}
}

func TestAudience(t *testing.T) {
	h := newHarness(llm.Scripted{Raw: `{"rules":[{"field":"city","op":"eq","value":"Paris"}],"unsupported_criteria":[]}`}, llm.Scripted{Raw: validAudience})
	a, err := h.c.Audience(t.Context(), campaignID, "İstanbul'dakiler", validator)
	if err != nil || a.Segment.Rules[0].Value != "İstanbul" {
		t.Fatalf("a = %+v, err = %v", a, err)
	}
	if len(h.mock.Calls()) != 2 || h.mock.Calls()[0].OnDelta != nil {
		t.Error("audience must repair once and not stream")
	}
	if c := h.rec.calls; len(c) != 2 || c[0].Step != llm.StepAudience || c[0].ValidationErrors[0].Code != segment.CodeUnknownCity {
		t.Errorf("recorded = %+v", c)
	}
}

// Nothing expressible, with a reason: final on the first answer, no repair.
func TestAudienceUnmappable(t *testing.T) {
	h := newHarness(llm.Scripted{Raw: `{"rules":[],"unsupported_criteria":["30 yaş üstü"]}`}, llm.Scripted{Raw: validAudience})
	a, err := h.c.Audience(t.Context(), campaignID, "30 yaş üstü kullanıcılar", validator)
	if !errors.Is(err, prompt.ErrUnmappable) || len(a.Unsupported) != 1 {
		t.Fatalf("a = %+v, err = %v", a, err)
	}
	if h.mock.Remaining() != 1 {
		t.Error("unmappable answer was repaired")
	}
}

func TestAudienceExhausted(t *testing.T) {
	paris := llm.Scripted{Raw: `{"rules":[{"field":"city","op":"eq","value":"Paris"}],"unsupported_criteria":[]}`}
	h := newHarness(paris, paris, paris)
	_, err := h.c.Audience(t.Context(), campaignID, "Paris", validator)
	var ex *ExhaustedError
	if !errors.As(err, &ex) || ex.Step != llm.StepAudience || ex.Errors[0].Code != segment.CodeUnknownCity {
		t.Fatalf("err = %v, want ExhaustedError with unknown_city", err)
	}
}

func TestBackoffJitter(t *testing.T) {
	c := New(llm.NewMock(), nil, DefaultPolicy())
	c.jitter = func(d time.Duration) time.Duration { return d }
	if got := []time.Duration{c.backoff(1), c.backoff(2), c.backoff(3)}; !slices.Equal(got, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Errorf("backoff = %v", got)
	}
}

// The keyless demo path: the mock in fixture mode through the whole pipeline.
func TestWithFixtureMock(t *testing.T) {
	c := New(llm.NewMock(), nil, DefaultPolicy())
	a, err := c.Audience(t.Context(), 1, llm.ExampleBriefTR, validator)
	if err != nil || len(a.Segment.Rules) != 3 {
		t.Fatalf("audience = %+v, %v", a, err)
	}
	res, err := c.Messages(t.Context(), 1, prompt.MessagesInput{Brief: llm.ExampleBriefTR, Segment: a.Segment, LongestFirstName: 15}, nil)
	if err != nil || !res.Valid {
		t.Fatalf("messages = %+v, %v", res, err)
	}
}
