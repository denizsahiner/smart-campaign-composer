package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func userReq(step Step, brief string) Request {
	return Request{Step: step, Messages: []Message{{Role: RoleUser, Content: "Brief:\n" + brief}}}
}

func TestMockFixtureByBrief(t *testing.T) {
	m := NewMock()
	ctx := context.Background()

	// Matching ignores case and whitespace, and the brief may be wrapped in a prompt.
	messy := strings.ToUpper(strings.ReplaceAll(ExampleBriefEN, " ", "  \n"))
	res, err := m.Generate(ctx, userReq(StepAudience, messy))
	if err != nil || string(res.Raw) != exampleAudience {
		t.Fatalf("audience = %s, %v", res.Raw, err)
	}
	res, err = m.Generate(ctx, userReq(StepMessages, ExampleBriefTR))
	if err != nil || !strings.Contains(string(res.Raw), "Merhaba") {
		t.Fatalf("TR messages = %s, %v", res.Raw, err)
	}
	res, err = m.Generate(ctx, userReq(StepMessages, ExampleBriefEN))
	if err != nil || !strings.Contains(string(res.Raw), "Hi {{first_name}}") {
		t.Fatalf("EN messages = %s, %v", res.Raw, err)
	}
	res, err = m.Generate(ctx, userReq(StepAudience, "something else entirely"))
	if err != nil || string(res.Raw) != defaultFixture.Audience {
		t.Fatalf("default = %s, %v", res.Raw, err)
	}
	if _, err := m.Generate(ctx, userReq("bogus", ExampleBriefEN)); err == nil {
		t.Fatal("unknown step: want error")
	}
}

func TestMockScriptedSequence(t *testing.T) {
	busy := &TransportError{StatusCode: 529, Err: errors.New("overloaded")}
	m := NewScripted(
		Scripted{Raw: `{"rules":[`},
		Scripted{Err: busy},
		Scripted{Raw: `{"rules":[]}`},
	)
	ctx := context.Background()
	req := userReq(StepAudience, "x")

	if res, err := m.Generate(ctx, req); err != nil || string(res.Raw) != `{"rules":[` {
		t.Fatalf("1st = %s, %v (malformed JSON must come back raw, not as an error)", res.Raw, err)
	}
	if _, err := m.Generate(ctx, req); !errors.Is(err, busy) || !IsRetryable(err) {
		t.Fatalf("2nd err = %v, want retryable transport error", err)
	}
	if res, _ := m.Generate(ctx, req); string(res.Raw) != `{"rules":[]}` {
		t.Fatalf("3rd = %s", res.Raw)
	}
	if _, err := m.Generate(ctx, req); !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("4th err = %v, want ErrScriptExhausted", err)
	}
	if n := len(m.Calls()); n != 4 {
		t.Fatalf("recorded %d calls, want 4", n)
	}
}

// Calls records a snapshot: later mutation of the caller's slice must not change it.
func TestMockRecordsRequestSnapshot(t *testing.T) {
	m := NewScripted(Scripted{Raw: "{}"})
	msgs := []Message{{Role: RoleUser, Content: "first"}}
	m.Generate(context.Background(), Request{Step: StepAudience, Messages: msgs})
	msgs[0].Content = "changed"
	if got := m.Calls()[0].Messages[0].Content; got != "first" {
		t.Fatalf("recorded message = %q, want first", got)
	}
}

func TestMockStreamsOnRuneBoundaries(t *testing.T) {
	raw := `{"sms":"Şükrü, sepetindeki ürünler için %30 indirim!"}`
	m := NewScripted(Scripted{Raw: raw})
	m.ChunkRunes = 3
	var chunks []string
	res, err := m.Generate(context.Background(), Request{OnDelta: func(c string) { chunks = append(chunks, c) }})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(chunks, ""); got != raw || string(res.Raw) != raw {
		t.Fatalf("streamed %q, raw %q, want %q", got, res.Raw, raw)
	}
	for _, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %q is not valid UTF-8", c)
		}
	}
	if len(chunks) < 10 {
		t.Fatalf("got %d chunks, expected the reply to be streamed in pieces", len(chunks))
	}
}

// A stream that breaks mid-way delivers the partial output, then a Partial transport error.
func TestMockPartialStream(t *testing.T) {
	broken := &TransportError{Partial: true, Err: errors.New("connection reset")}
	m := NewScripted(Scripted{Raw: `{"sms":"Merhaba`, Err: broken})
	var got strings.Builder
	_, err := m.Generate(context.Background(), Request{OnDelta: func(c string) { got.WriteString(c) }})
	var te *TransportError
	if !errors.As(err, &te) || !te.Partial {
		t.Fatalf("err = %v, want partial TransportError", err)
	}
	if got.String() != `{"sms":"Merhaba` {
		t.Fatalf("streamed %q before the break", got.String())
	}
}

func TestMockStreamHonorsCancel(t *testing.T) {
	m := NewMock()
	m.ChunkRunes, m.ChunkDelay = 1, 50*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req := userReq(StepMessages, ExampleBriefEN)
	req.OnDelta = func(string) {}
	if _, err := m.Generate(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestTransportErrorRetryable(t *testing.T) {
	cases := map[int]bool{0: true, 429: true, 500: true, 529: true, 400: false, 401: false, 403: false, 404: false}
	for status, want := range cases {
		if got := (&TransportError{StatusCode: status}).Retryable(); got != want {
			t.Errorf("status %d: Retryable = %v, want %v", status, got, want)
		}
	}
	if IsRetryable(errors.New("plain")) {
		t.Error("plain error must not be retryable")
	}
}

func TestNewSelectsProvider(t *testing.T) {
	cases := []struct {
		cfg     Config
		want    string
		wantErr bool
	}{
		{Config{}, "mock", false},
		{Config{Provider: "mock", APIKey: "sk-x"}, "mock", false},
		{Config{Provider: "claude"}, "mock", false}, // no key → mock, so compose up works
		{Config{Provider: "claude", APIKey: "sk-x"}, "claude:claude-sonnet-5-5", false},
		{Config{APIKey: "sk-x"}, "claude:claude-sonnet-5-5", false}, // a key alone selects Claude
		{Config{APIKey: "sk-x", Model: "claude-opus-5-5"}, "claude:claude-opus-5-5", false},
		{Config{APIKey: "sk-x", EffortMessages: "extreme"}, "", true},
		{Config{Provider: "gpt", APIKey: "sk-x"}, "", true},
	}
	for _, c := range cases {
		_, name, err := New(c.cfg)
		if name != c.want || (err != nil) != c.wantErr {
			t.Errorf("New(%+v) = %q, %v", c.cfg, name, err)
		}
	}
}
