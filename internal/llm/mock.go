package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ErrScriptExhausted means a scripted Mock got more calls than it had responses: the code
// under test called the model more often than the test expected.
var ErrScriptExhausted = errors.New("llm mock: script exhausted")

// Scripted is one canned reply. Raw is returned (and streamed) as-is, so it can be
// malformed on purpose. If Err is set, Raw is streamed first and then Err is returned,
// which simulates a stream that breaks mid-way.
type Scripted struct {
	Raw string
	Err error
}

// Mock is a Client that never touches the network.
//
// In fixture mode (NewMock) it answers from fixtures keyed by step and brief; that's what
// runs under `docker compose up` without an API key. In scripted mode (NewScripted) it
// returns a fixed sequence of replies, for testing the validate → repair → retry loop.
// Either way the reply goes through the same parsing and validation as Claude's.
type Mock struct {
	// ChunkRunes and ChunkDelay control streaming through Request.OnDelta: the reply is
	// sent in chunks of ChunkRunes runes with ChunkDelay between them. Zero delay (the
	// test default) streams instantly.
	ChunkRunes int
	ChunkDelay time.Duration

	mu       sync.Mutex
	scripted bool
	script   []Scripted
	calls    []Request
}

func NewMock() *Mock { return &Mock{ChunkRunes: 16} }

func NewScripted(replies ...Scripted) *Mock {
	return &Mock{ChunkRunes: 16, scripted: true, script: replies}
}

func (m *Mock) Generate(ctx context.Context, req Request) (Response, error) {
	raw, replyErr, err := m.next(req)
	if err != nil {
		return Response{}, err
	}
	if err := m.stream(ctx, raw, req.OnDelta); err != nil {
		return Response{}, err
	}
	if replyErr != nil {
		return Response{}, replyErr
	}
	return Response{
		Raw:          []byte(raw),
		InputTokens:  approxTokens(req),
		OutputTokens: utf8.RuneCountInString(raw)/4 + 1,
		Model:        "mock",
	}, nil
}

// Calls returns the requests received so far, so tests can assert on what was sent
// (e.g. that a repair round included the validation errors).
func (m *Mock) Calls() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.calls...)
}

// Remaining returns the number of unused scripted replies.
func (m *Mock) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.script)
}

func (m *Mock) next(req Request) (raw string, replyErr, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	req.Messages = append([]Message(nil), req.Messages...) // caller may reuse the slice
	m.calls = append(m.calls, req)

	if !m.scripted {
		raw, err := fixtureFor(req)
		return raw, nil, err
	}
	if len(m.script) == 0 {
		return "", nil, ErrScriptExhausted
	}
	r := m.script[0]
	m.script = m.script[1:]
	return r.Raw, r.Err, nil
}

func (m *Mock) stream(ctx context.Context, raw string, onDelta func(string)) error {
	if onDelta == nil {
		return ctx.Err()
	}
	size := max(m.ChunkRunes, 1)
	for len(raw) > 0 {
		// Cut on a rune boundary: splitting "ş" across chunks would hand the UI invalid UTF-8.
		end, n := 0, 0
		for end < len(raw) && n < size {
			_, w := utf8.DecodeRuneInString(raw[end:])
			end += w
			n++
		}
		if m.ChunkDelay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.ChunkDelay):
			}
		} else if err := ctx.Err(); err != nil {
			return err
		}
		onDelta(raw[:end])
		raw = raw[end:]
	}
	return nil
}

func approxTokens(req Request) int {
	n := utf8.RuneCountInString(req.System)
	for _, msg := range req.Messages {
		n += utf8.RuneCountInString(msg.Content)
	}
	return n/4 + 1
}

// fixtureFor picks the canned reply for a request. The brief is looked for in the first
// user message, so it works whatever prompt template wraps it.
func fixtureFor(req Request) (string, error) {
	var first string
	for _, msg := range req.Messages {
		if msg.Role == RoleUser {
			first = msg.Content
			break
		}
	}
	f := defaultFixture
	text := normalizeBrief(first)
	for _, cand := range fixtures {
		if strings.Contains(text, normalizeBrief(cand.Brief)) {
			f = cand
			break
		}
	}
	switch req.Step {
	case StepAudience:
		return f.Audience, nil
	case StepMessages:
		return f.Messages, nil
	default:
		return "", fmt.Errorf("llm mock: unknown step %q", req.Step)
	}
}

// normalizeBrief makes matching tolerant of case and whitespace differences.
func normalizeBrief(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
