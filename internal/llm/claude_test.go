package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// SSE frames in the Messages streaming format.
func ev(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

var (
	msgStart  = ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":321,"output_tokens":1}}}`)
	textStart = ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
	thinking  = ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"secret plan"}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`) +
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	msgStop = ev("message_stop", `{"type":"message_stop"}`)
)

func textDelta(s string) string {
	b, _ := json.Marshal(s)
	return ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":`+string(b)+`}}`)
}

func msgDelta(stopReason, stopDetails string) string {
	if stopDetails == "" {
		stopDetails = "null"
	}
	return ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stopReason+`","stop_sequence":null,"stop_details":`+stopDetails+`},"usage":{"output_tokens":57}}`)
}

// stubAPI serves one canned response per request and records the request bodies.
type stubAPI struct {
	srv    *httptest.Server
	hits   atomic.Int32
	bodies []map[string]any
	header http.Header
}

func newStub(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, flush func())) *stubAPI {
	t.Helper()
	s := &stubAPI{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		s.bodies = append(s.bodies, body)
		s.header = r.Header.Clone()
		flush := func() { w.(http.Flusher).Flush() }
		handle(w, r, flush)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func streamOK(frames ...string) func(http.ResponseWriter, *http.Request, func()) {
	return func(w http.ResponseWriter, r *http.Request, flush func()) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			io.WriteString(w, f)
			flush()
		}
	}
}

func httpError(status int, errType string, headers map[string]string) func(http.ResponseWriter, *http.Request, func()) {
	return func(w http.ResponseWriter, _ *http.Request, _ func()) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":"stub"}}`, errType)
	}
}

// hang blocks like a stalled server until the client goes away.
func hang(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

func testClaude(s *stubAPI, mutate ...func(*ClaudeConfig)) *Claude {
	cfg := DefaultClaudeConfig()
	cfg.APIKey = "sk-test"
	cfg.BaseURL = s.srv.URL
	for _, m := range mutate {
		m(&cfg)
	}
	return NewClaude(cfg)
}

var testReq = Request{
	Step:     StepMessages,
	System:   "You write campaigns.",
	Messages: []Message{{Role: RoleUser, Content: "<brief>x</brief>"}},
	Schema:   []byte(`{"type":"object","properties":{"sms":{"type":"string"}},"required":["sms"],"additionalProperties":false}`),
}

func TestClaudeStreamsAnswer(t *testing.T) {
	s := newStub(t, streamOK(msgStart, thinking, textStart,
		textDelta(`{"sms":"Merhaba `), textDelta(`{{first_name}}, şimdi %30 indirim!"}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":1}`), msgDelta("end_turn", ""), msgStop))
	var chunks []string
	req := testReq
	req.OnDelta = func(c string) { chunks = append(chunks, c) }

	res, err := testClaude(s).Generate(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sms":"Merhaba {{first_name}}, şimdi %30 indirim!"}`
	if string(res.Raw) != want || strings.Join(chunks, "") != want {
		t.Fatalf("raw = %s, streamed = %q", res.Raw, chunks)
	}
	if res.InputTokens != 321 || res.OutputTokens != 57 || res.Model != "claude-opus-5-5" {
		t.Errorf("res = %+v", res)
	}
	if strings.Contains(string(res.Raw), "secret") {
		t.Error("thinking text leaked into the answer")
	}
}

// The request uses structured outputs, never tools or forced tool_choice.
func TestClaudeRequestShape(t *testing.T) {
	s := newStub(t, streamOK(msgStart, textStart, textDelta(`{}`), msgDelta("end_turn", ""), msgStop))
	if _, err := testClaude(s).Generate(t.Context(), testReq); err != nil {
		t.Fatal(err)
	}
	b := s.bodies[0]
	if b["model"] != "claude-sonnet-5-5" || b["stream"] != true || b["max_tokens"].(float64) != 16000 {
		t.Errorf("model/stream/max_tokens: %v %v %v", b["model"], b["stream"], b["max_tokens"])
	}
	if _, ok := b["tools"]; ok {
		t.Error("request must not define tools")
	}
	if _, ok := b["tool_choice"]; ok {
		t.Error("request must not set tool_choice")
	}
	if _, ok := b["thinking"]; ok {
		t.Error("thinking must be left to the model default")
	}
	oc := b["output_config"].(map[string]any)
	format := oc["format"].(map[string]any)
	if oc["effort"] != "medium" || format["type"] != "json_schema" || format["schema"].(map[string]any)["additionalProperties"] != false {
		t.Errorf("output_config = %v", oc)
	}
	if sys := b["system"].([]any)[0].(map[string]any)["text"]; sys != "You write campaigns." {
		t.Errorf("system = %v", sys)
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("messages = %v", msgs)
	}
	if b["fallbacks"] != "default" || !strings.Contains(s.header.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("fallbacks = %v, beta = %q", b["fallbacks"], s.header.Get("Anthropic-Beta"))
	}
	if s.header.Get("Authorization") != "Bearer sk-test" && s.header.Get("X-Api-Key") != "sk-test" {
		t.Error("API key not sent")
	}

	// Audience step effort; Haiku gets no fallback parameter (it doesn't support one).
	s2 := newStub(t, streamOK(msgStart, textStart, textDelta(`{}`), msgDelta("end_turn", ""), msgStop))
	req := testReq
	req.Step = StepAudience
	testClaude(s2, func(c *ClaudeConfig) { c.Model = "claude-haiku-5-5" }).Generate(t.Context(), req)
	b = s2.bodies[0]
	if b["output_config"].(map[string]any)["effort"] != "low" {
		t.Errorf("audience effort = %v", b["output_config"])
	}
	if _, ok := b["fallbacks"]; ok || strings.Contains(s2.header.Get("Anthropic-Beta"), "fallback") {
		t.Error("haiku request must not ask for fallbacks")
	}
}

func TestClaudeHTTPErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		errType    string
		headers    map[string]string
		retryable  bool
		retryAfter time.Duration
	}{
		{"rate limited", 429, "rate_limit_error", map[string]string{"Retry-After": "7"}, true, 7 * time.Second},
		{"overloaded", 529, "overloaded_error", nil, true, 0},
		{"server error", 500, "api_error", nil, true, 0},
		{"bad request", 400, "invalid_request_error", nil, false, 0},
		{"bad key", 401, "authentication_error", nil, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStub(t, httpError(c.status, c.errType, c.headers))
			_, err := testClaude(s).Generate(t.Context(), testReq)
			var te *TransportError
			if !errors.As(err, &te) || te.StatusCode != c.status || te.Retryable() != c.retryable || te.RetryAfter != c.retryAfter || te.Partial {
				t.Fatalf("err = %#v", err)
			}
			if n := s.hits.Load(); n != 1 {
				t.Errorf("server hit %d times; SDK retries must be off", n)
			}
		})
	}
}

// An error event inside a 200 stream after output started: retryable, and Partial so the
// UI discards the half-drawn draft.
func TestClaudeMidStreamError(t *testing.T) {
	s := newStub(t, streamOK(msgStart, textStart, textDelta(`{"sms":"Mer`),
		ev("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)))
	_, err := testClaude(s).Generate(t.Context(), testReq)
	var te *TransportError
	if !errors.As(err, &te) || te.StatusCode != 529 || !te.Partial || !te.Retryable() {
		t.Fatalf("err = %#v", err)
	}
}

func TestClaudeRefusal(t *testing.T) {
	s := newStub(t, streamOK(msgStart, msgDelta("refusal", `{"type":"refusal","category":"cyber","explanation":"nope"}`), msgStop))
	_, err := testClaude(s).Generate(t.Context(), testReq)
	var re *RefusalError
	if !errors.As(err, &re) || re.Category != "cyber" || re.Explanation != "nope" {
		t.Fatalf("err = %#v", err)
	}
	if IsRetryable(err) {
		t.Error("a refusal must not be retryable")
	}
}

func TestClaudeTruncated(t *testing.T) {
	s := newStub(t, streamOK(msgStart, textStart, textDelta(`{"sms":"Merhaba`), msgDelta("max_tokens", ""), msgStop))
	if _, err := testClaude(s).Generate(t.Context(), testReq); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestClaudeTimeouts(t *testing.T) {
	short := func(c *ClaudeConfig) { c.FirstEventTimeout, c.IdleTimeout = 150*time.Millisecond, 150*time.Millisecond }

	t.Run("never starts", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, _ func()) { hang(r) })
		start := time.Now()
		_, err := testClaude(s, short).Generate(t.Context(), testReq)
		var te *TransportError
		if !errors.As(err, &te) || !errors.Is(err, errFirstEventTimeout) || te.Partial || !te.Retryable() {
			t.Fatalf("err = %#v", err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("took %v; the first-event timeout didn't fire", time.Since(start))
		}
	})

	t.Run("stalls mid-stream", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, flush func()) {
			streamOK(msgStart, textStart, textDelta(`{"sms":"Mer`))(w, r, flush)
			hang(r)
		})
		start := time.Now()
		_, err := testClaude(s, short).Generate(t.Context(), testReq)
		var te *TransportError
		if !errors.As(err, &te) || !errors.Is(err, errIdleTimeout) || !te.Partial {
			t.Fatalf("err = %#v", err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("took %v; the idle timeout didn't fire", time.Since(start))
		}
	})

	// A slow but steady stream is fine: the idle timer restarts on every event.
	t.Run("slow but alive", func(t *testing.T) {
		s := newStub(t, func(w http.ResponseWriter, r *http.Request, flush func()) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, f := range []string{msgStart, textStart, textDelta(`{"sms":`), textDelta(`"a"}`), msgDelta("end_turn", ""), msgStop} {
				io.WriteString(w, f)
				flush()
				time.Sleep(100 * time.Millisecond)
			}
		})
		if _, err := testClaude(s, short).Generate(t.Context(), testReq); err != nil {
			t.Fatalf("a stream with 100ms gaps and a 150ms idle timeout failed: %v", err)
		}
	})
}

// The caller going away is reported as cancellation, not as a transport failure to retry.
func TestClaudeCallerCancel(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, r *http.Request, flush func()) {
		streamOK(msgStart, textStart)(w, r, flush)
		hang(r)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := testClaude(s).Generate(ctx, testReq)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %#v, want the caller's context error", err)
	}
	var te *TransportError
	if errors.As(err, &te) {
		t.Error("caller cancellation must not look like a transport error")
	}
}

func TestRetryAfterHeader(t *testing.T) {
	h := http.Header{}
	if retryAfter(h) != 0 {
		t.Error("absent header")
	}
	h.Set("Retry-After", "2.5")
	if retryAfter(h) != 2500*time.Millisecond {
		t.Errorf("seconds: %v", retryAfter(h))
	}
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	if d := retryAfter(h); d < 28*time.Second || d > 31*time.Second {
		t.Errorf("http date: %v", d)
	}
	h.Set("Retry-After", "soon")
	if retryAfter(h) != 0 {
		t.Error("garbage")
	}
}
