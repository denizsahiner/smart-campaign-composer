package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func jsonLogger(buf *bytes.Buffer) *slog.Logger {
	return New(Config{Service: "api", Level: slog.LevelDebug, Format: "json", Out: buf})
}

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not a JSON line: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func TestEveryRecordHasTheService(t *testing.T) {
	var buf bytes.Buffer
	jsonLogger(&buf).Info("hello")
	if got := lines(t, &buf)[0]["service"]; got != "api" {
		t.Fatalf("service = %v, want api", got)
	}
}

func TestDurationsAreText(t *testing.T) {
	var buf bytes.Buffer
	jsonLogger(&buf).Info("x", "duration", 1500*time.Millisecond)
	if got := lines(t, &buf)[0]["duration"]; got != "1.5s" {
		t.Fatalf("duration = %v, want 1.5s", got)
	}
}

func TestScopeAttributesAreAdded(t *testing.T) {
	var buf bytes.Buffer
	log := jsonLogger(&buf)
	ctx := WithScope(context.Background())
	Add(ctx, "request_id", "abc")
	Add(ctx, "campaign_id", int64(7))
	Add(ctx, "campaign_id", int64(8)) // replaces

	log.InfoContext(ctx, "one")
	log.InfoContext(ctx, "two", "campaign_id", int64(9))         // the call's own value wins
	log.With("campaign_id", int64(10)).InfoContext(ctx, "three") // so does the logger's
	log.Info("four")                                             // no context: no scope

	got := lines(t, &buf)
	if got[0]["request_id"] != "abc" || got[0]["campaign_id"] != float64(8) {
		t.Errorf("one = %v", got[0])
	}
	for i, want := range []float64{9, 10} {
		raw, _ := json.Marshal(got[i+1])
		if strings.Count(string(raw), `"campaign_id"`) != 1 || got[i+1]["campaign_id"] != want {
			t.Errorf("line %d = %s, want one campaign_id %v", i+2, raw, want)
		}
	}
	if _, ok := got[3]["request_id"]; ok {
		t.Errorf("four has a request_id: %v", got[3])
	}
	if RequestID(ctx) != "abc" || RequestID(context.Background()) != "" {
		t.Errorf("RequestID = %q", RequestID(ctx))
	}
}

func TestAddWithoutScopeIsHarmless(t *testing.T) {
	Add(context.Background(), "campaign_id", 1)
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("LOG_LEVEL", "warn")
	t.Setenv("LOG_FORMAT", "TEXT")
	cfg, err := ConfigFromEnv("worker")
	if err != nil || cfg.Level != slog.LevelWarn || cfg.Format != "text" || cfg.Service != "worker" {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
	t.Setenv("LOG_LEVEL", "loud")
	if _, err := ConfigFromEnv("worker"); err == nil {
		t.Error("LOG_LEVEL=loud accepted")
	}
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("LOG_FORMAT", "xml")
	if _, err := ConfigFromEnv("worker"); err == nil {
		t.Error("LOG_FORMAT=xml accepted")
	}
}

func TestNewRequestID(t *testing.T) {
	if id := NewRequestID("from-proxy_1.2"); id != "from-proxy_1.2" {
		t.Errorf("a valid incoming ID was replaced: %q", id)
	}
	for _, bad := range []string{"", "has space", "<script>", strings.Repeat("a", 65)} {
		id := NewRequestID(bad)
		if id == bad || len(id) != 16 {
			t.Errorf("NewRequestID(%q) = %q, want a fresh 16-char ID", bad, id)
		}
	}
	if NewRequestID("") == NewRequestID("") {
		t.Error("two fresh IDs are equal")
	}
}

// The sink's handler: columns pulled out, the rest as JSON-friendly attrs, warn+ only.
func TestSinkHandlerEntries(t *testing.T) {
	s := &Sink{entries: make(chan entry, 10)}
	var buf bytes.Buffer
	log := New(Config{Service: "worker", Level: slog.LevelDebug, Out: &buf}, s.Handler(slog.LevelInfo))
	ctx := WithScope(context.Background())
	Add(ctx, "request_id", "r1")

	log.InfoContext(ctx, "not stored")
	log.ErrorContext(ctx, "message failed", "err", errors.New("boom"), "retry_in", 1500*time.Millisecond)
	log.WithGroup("send").With("channel", "sms").Error("grouped", "campaign_id", int64(12), "ok", true)
	log.Warn("throttled", "campaign_id", "not-a-number")

	if len(s.entries) != 3 {
		t.Fatalf("%d entries, want 3 (info is not stored)", len(s.entries))
	}
	e := <-s.entries
	if e.level != "error" || e.service != "worker" || e.msg != "message failed" ||
		e.requestID == nil || *e.requestID != "r1" || e.attrs["err"] != "boom" || e.attrs["retry_in"] != "1.5s" {
		t.Errorf("entry = %+v", e)
	}
	// Grouped attributes nest, from the logger and the call alike; a campaign_id inside a
	// group isn't the column.
	e = <-s.entries
	send, _ := e.attrs["send"].(map[string]any)
	if e.campaignID != nil || send["channel"] != "sms" || send["campaign_id"] != int64(12) || send["ok"] != true {
		t.Errorf("grouped entry = %+v", e)
	}
	e = <-s.entries
	if e.level != "warn" || e.campaignID != nil || e.attrs["campaign_id"] != "not-a-number" {
		t.Errorf("entry = %+v", e)
	}
}

func TestSinkCampaignColumn(t *testing.T) {
	s := &Sink{entries: make(chan entry, 1)}
	slog.New(s.Handler(slog.LevelWarn)).Error("x", "campaign_id", int64(5), "err", errors.New("e"))
	e := <-s.entries
	if e.campaignID == nil || *e.campaignID != 5 || e.attrs["err"] != "e" {
		t.Fatalf("entry = %+v", e)
	}
	if _, ok := e.attrs["campaign_id"]; ok {
		t.Error("campaign_id also in attrs")
	}
}

func TestSinkDropsWhenFull(t *testing.T) {
	s := &Sink{entries: make(chan entry, 1)}
	log := slog.New(s.Handler(slog.LevelWarn))
	log.Error("a")
	log.Error("b")
	if s.dropped.Load() != 1 {
		t.Fatalf("dropped = %d, want 1", s.dropped.Load())
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("ğ", maxValueRunes+5)
	got := truncate(long)
	if n := len([]rune(got)); n != maxValueRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated to %d runes", n)
	}
}
