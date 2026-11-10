package api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/logging"
)

func (e *env) insertLog(level, service, msg string, campaignID any, requestID any, age time.Duration, attrs string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO app_logs (ts, level, service, msg, campaign_id, request_id, attrs)
		VALUES (now() - $6 * interval '1 second', $1, $2, $3, $4, $5, $7)`,
		level, service, msg, campaignID, requestID, int64(age/time.Second), attrs); err != nil {
		e.t.Fatal(err)
	}
}

func messages(body map[string]any) []string {
	var out []string
	for _, l := range body["logs"].([]any) {
		out = append(out, l.(map[string]any)["message"].(string))
	}
	return out
}

func TestListLogs(t *testing.T) {
	e := newEnv(t)
	e.insertLog("warn", "worker", "send failed, retry scheduled", 7, nil, 3*time.Hour, `{"err":"server_error"}`)
	e.insertLog("error", "worker", "message failed", 7, nil, 2*time.Hour, `{"err":"invalid_recipient"}`)
	e.insertLog("error", "api", "request failed", nil, "req-1", time.Hour, `{"path":"/api/metrics"}`)
	e.insertLog("warn", "api", "llm refused", 8, "req-2", 0, `{}`)

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{"llm refused", "request failed", "message failed", "send failed, retry scheduled"}},
		{"?level=error", []string{"request failed", "message failed"}},
		{"?campaign_id=7", []string{"message failed", "send failed, retry scheduled"}},
		{"?campaign_id=7&level=error", []string{"message failed"}},
		{"?service=api", []string{"llm refused", "request failed"}},
		{"?request_id=req-1", []string{"request failed"}},
		{"?q=RECIPIENT", []string{"message failed"}}, // in attrs, case-insensitive
		{"?q=refused", []string{"llm refused"}},      // in the message
		{"?q=50%25", nil},                            // % is literal
	} {
		status, body := e.do("GET", "/api/logs"+tc.query, nil)
		if status != 200 {
			t.Fatalf("%s: %d %v", tc.query, status, body)
		}
		if got := messages(body); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: %v, want %v", tc.query, got, tc.want)
		}
	}

	// Paging: newest first, next_before continues.
	_, body := e.do("GET", "/api/logs?limit=3", nil)
	if len(body["logs"].([]any)) != 3 || body["next_before"] == nil {
		t.Fatalf("page 1: %v", body)
	}
	_, body = e.do("GET", fmt.Sprintf("/api/logs?limit=3&before=%.0f", body["next_before"].(float64)), nil)
	if got := messages(body); fmt.Sprint(got) != "[send failed, retry scheduled]" || body["next_before"] != nil {
		t.Errorf("page 2: %v", body)
	}

	for _, q := range []string{"?level=info", "?campaign_id=x", "?limit=0", "?limit=501", "?before=x"} {
		if status, body := e.do("GET", "/api/logs"+q, nil); status != 400 || errCode(body) != "bad_request" {
			t.Errorf("%s: %d %v", q, status, body)
		}
	}
}

func TestLogSummary(t *testing.T) {
	e := newEnv(t)
	e.insertLog("error", "worker", "message failed", 7, nil, 0, `{}`)
	e.insertLog("error", "worker", "message failed", 7, nil, 30*time.Hour, `{}`)
	e.insertLog("warn", "worker", "send failed, retry scheduled", 7, nil, 0, `{}`)
	e.insertLog("error", "api", "request failed", 8, nil, 0, `{}`)

	for q, want := range map[string][2]float64{
		"":                              {3, 1},
		"?campaign_id=7":                {2, 1},
		"?campaign_id=7&since_hours=24": {1, 1},
		"?campaign_id=9":                {0, 0},
	} {
		status, body := e.do("GET", "/api/logs/summary"+q, nil)
		if status != 200 || body["errors"] != want[0] || body["warnings"] != want[1] {
			t.Errorf("%s: %d %v, want errors %v warnings %v", q, status, body, want[0], want[1])
		}
	}
	if status, _ := e.do("GET", "/api/logs/summary?since_hours=0", nil); status != 400 {
		t.Errorf("since_hours=0: %d", status)
	}
}

// End to end: a failed LLM step is stored with its campaign and request, so the
// campaign page can point at it.
func TestLLMFailureIsLoggedForTheCampaign(t *testing.T) {
	busy := llm.Scripted{Err: &llm.TransportError{StatusCode: 529, Err: io.EOF}}
	e := newEnv(t, llm.Scripted{Raw: audienceOK}, busy, busy, busy)

	cfg := logging.Config{Service: "api", Level: slog.LevelInfo, Out: io.Discard}
	sink := logging.NewSink(e.pool, cfg.Console())
	prev := slog.Default()
	slog.SetDefault(logging.New(cfg, sink.Handler(cfg.Level)))
	defer slog.SetDefault(prev)

	id, _ := e.create("İstanbul'da son 30 günde sepete ekleyenler")
	req, _ := http.NewRequest("POST", e.srv.URL+path(id, "/audience"), bytes.NewReader([]byte(`{"brief":"yeni brief"}`)))
	req.Header.Set("X-Request-ID", "trace-me")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Request-ID") != "trace-me" {
		t.Errorf("X-Request-ID = %q", resp.Header.Get("X-Request-ID"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sink.Close(ctx)

	_, body := e.do("GET", fmt.Sprintf("/api/logs?campaign_id=%d&level=error", id), nil)
	logs := body["logs"].([]any)
	if len(logs) != 1 {
		t.Fatalf("logs = %v, want the failed step", body)
	}
	l := logs[0].(map[string]any)
	attrs := l["attrs"].(map[string]any)
	if l["message"] != "llm step failed" || l["request_id"] != "trace-me" || l["service"] != "api" || attrs["step"] != "audience" {
		t.Errorf("log = %v", l)
	}
	_, body = e.do("GET", fmt.Sprintf("/api/logs?campaign_id=%d&level=warn", id), nil)
	if got := messages(body); len(got) != 3 || got[1] != "llm call failed, retrying" {
		t.Errorf("warn and above = %v, want the failure and two retries", got)
	}
}
