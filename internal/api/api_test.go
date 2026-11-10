package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denizsahiner/smart-campaign-composer/internal/api"
	"github.com/denizsahiner/smart-campaign-composer/internal/compose"
	"github.com/denizsahiner/smart-campaign-composer/internal/dbtest"
	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/prompt"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
	"github.com/denizsahiner/smart-campaign-composer/internal/store"
)

const (
	validTR    = `{"sms":"Merhaba {{first_name}}, sepetin seni bekliyor!","email_subject":"Sepetin %30 indirimli","email_body":"Merhaba {{first_name}},\n\nSepetin seni bekliyor."}`
	validEN    = `{"sms":"Hi {{first_name}}, your cart is waiting!","email_subject":"30% off your cart","email_body":"Hi {{first_name}},\n\nYour cart is waiting."}`
	audienceOK = `{"rules":[{"field":"city","op":"eq","value":"istanbul"},{"field":"cart_added_days_ago","op":"lte","value":30}],"unsupported_criteria":[]}`
)

// The scripted audiences have recipients in both languages, so a messages answer has both.
var (
	validMessages   = `{"tr":` + validTR + `,"en":` + validEN + `}`
	tooLongMessages = `{"tr":{"sms":"Merhaba {{first_name}}, ` + strings.Repeat("çok uzun mesaj var ", 8) + `","email_subject":"Konu","email_body":"Gövde"},"en":` + validEN + `}`
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	server *api.Server
	pool   *pgxpool.Pool
	mock   *llm.Mock
}

// newEnv starts the API on a seeded database. With no replies the mock answers from its
// fixtures; otherwise it plays the replies in order.
func newEnv(t *testing.T, replies ...llm.Scripted) *env {
	t.Helper()
	pool, _, _ := dbtest.Seeded(t, time.Now())
	cities, err := segment.KnownCities(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	mock := llm.NewMock()
	if len(replies) > 0 {
		mock = llm.NewScripted(replies...)
	}
	policy := compose.DefaultPolicy()
	policy.BaseBackoff = time.Millisecond
	composer := compose.New(mock, store.New(pool), policy)
	monitoring := api.DefaultMonitoring()
	monitoring.PollInterval = 20 * time.Millisecond
	server := api.NewServer(pool, composer, segment.NewValidator(cities)).WithMonitoring(monitoring)
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, server: server, pool: pool, mock: mock}
}

func (e *env) do(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) create(brief string) (int64, map[string]any) {
	e.t.Helper()
	status, body := e.do("POST", "/api/campaigns", map[string]string{"brief": brief})
	if status != http.StatusCreated {
		e.t.Fatalf("create: %d %v", status, body)
	}
	c := body["campaign"].(map[string]any)
	return int64(c["id"].(float64)), body
}

func (e *env) get(id int64) map[string]any {
	e.t.Helper()
	status, body := e.do("GET", path(id, ""), nil)
	if status != http.StatusOK {
		e.t.Fatalf("get: %d %v", status, body)
	}
	return body
}

func path(id int64, suffix string) string {
	b, _ := json.Marshal(id)
	return "/api/campaigns/" + string(b) + suffix
}

type sseEvent struct {
	name string
	data map[string]any
}

// stream posts to the messages endpoint and reads every event. A non-200 answer is
// returned as status with the JSON error and no events.
func (e *env) stream(id int64, body any) (int, []sseEvent, map[string]any) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(e.srv.URL+path(id, "/messages"), "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, nil, out
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		e.t.Fatalf("content type %q", ct)
	}
	return resp.StatusCode, readEvents(e.t, resp.Body), nil
}

func readEvents(t *testing.T, r io.Reader) []sseEvent {
	t.Helper()
	var out []sseEvent
	sc := bufio.NewScanner(r)
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data); err != nil {
				t.Fatalf("bad event data %q: %v", line, err)
			}
		case line == "":
			out = append(out, cur)
			cur = sseEvent{}
		}
	}
	return out
}

func names(evs []sseEvent) []string {
	var out []string
	for _, e := range evs {
		if e.name != "delta" {
			out = append(out, e.name)
		}
	}
	return out
}

func errCode(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func (e *env) llmCalls(id int64) []map[string]any {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT step, prompt_version, model, repair_attempt, validation_errors IS NOT NULL, error IS NOT NULL, raw_response IS NOT NULL
		FROM llm_calls WHERE campaign_id = $1 ORDER BY id`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var step, version string
		var model *string
		var repair int
		var hasVErr, hasErr, hasRaw bool
		rows.Scan(&step, &version, &model, &repair, &hasVErr, &hasErr, &hasRaw)
		out = append(out, map[string]any{"step": step, "version": version, "model": model, "repair": repair, "invalid": hasVErr, "error": hasErr, "raw": hasRaw})
	}
	return out
}

func TestFullFlow(t *testing.T) {
	e := newEnv(t)
	id, body := e.create(llm.ExampleBriefTR)
	if body["audience_error"] != nil {
		t.Fatalf("audience error: %v", body["audience_error"])
	}
	c := body["campaign"].(map[string]any)
	a := c["audience"].(map[string]any)
	reach := a["reach"].(map[string]any)
	if c["status"] != "draft" || len(a["rules"].([]any)) != 3 || reach["total"].(float64) < 400 ||
		reach["sms"].(float64) == 0 || reach["email"].(float64) == 0 || reach["longest_first_name"].(float64) != 15 {
		t.Fatalf("campaign = %v", c)
	}
	if d := a["description"].([]any); d[0] != "lives in İstanbul" {
		t.Errorf("description = %v", d)
	}
	langs := reach["languages"].(map[string]any)
	if fmt.Sprint(a["content_languages"]) != "[tr en]" || langs["tr"].(map[string]any)["total"].(float64)+langs["en"].(map[string]any)["total"].(float64) != reach["total"] {
		t.Errorf("languages = %v, reach = %v", a["content_languages"], reach)
	}

	status, evs, _ := e.stream(id, map[string]any{})
	if status != 200 || evs[0].name != "delta" || names(evs)[len(names(evs))-1] != "validated" {
		t.Fatalf("stream: %d %v", status, names(evs))
	}
	last := evs[len(evs)-1].data["contents"].(map[string]any)

	c = e.get(id)
	contents := c["contents"].(map[string]any)
	if c["status"] != "ready" || contents["tr"].(map[string]any)["sms"] != last["tr"].(map[string]any)["sms"] || c["content_errors"] != nil {
		t.Fatalf("after generation: %v", c)
	}
	if en := contents["en"].(map[string]any)["sms"].(string); !strings.HasPrefix(en, "Hi") {
		t.Errorf("English SMS = %q", en)
	}

	calls := e.llmCalls(id)
	if len(calls) != 2 || calls[0]["step"] != "audience" || calls[1]["step"] != "messages" ||
		*calls[1]["model"].(*string) != "mock" || calls[1]["invalid"] != false || calls[1]["raw"] != true {
		t.Errorf("llm_calls = %v", calls)
	}

	status, list := e.do("GET", "/api/campaigns", nil)
	if status != 200 || len(list["campaigns"].([]any)) != 1 {
		t.Errorf("list: %d %v", status, list)
	}
}

func TestUnsupportedMustBeAcknowledged(t *testing.T) {
	e := newEnv(t,
		llm.Scripted{Raw: `{"rules":[{"field":"city","op":"eq","value":"Ankara"}],"unsupported_criteria":["30 yaş üstü"]}`},
		llm.Scripted{Raw: validMessages},
	)
	id, body := e.create("30 yaş üstü Ankaralılar")
	a := body["campaign"].(map[string]any)["audience"].(map[string]any)
	if a["unsupported_criteria"].([]any)[0] != "30 yaş üstü" || a["unsupported_acknowledged"] != false {
		t.Fatalf("audience = %v", a)
	}

	status, _, errBody := e.stream(id, map[string]any{})
	if status != http.StatusConflict || errCode(errBody) != "unsupported_not_acknowledged" {
		t.Fatalf("before ack: %d %v", status, errBody)
	}
	status, c := e.do("POST", path(id, "/acknowledge"), nil)
	if status != 200 || c["audience"].(map[string]any)["unsupported_acknowledged"] != true {
		t.Fatalf("ack: %d %v", status, c)
	}
	status, evs, _ := e.stream(id, map[string]any{})
	if status != 200 || names(evs)[len(names(evs))-1] != "validated" {
		t.Fatalf("after ack: %d %v", status, names(evs))
	}
}

func TestUnmappableBrief(t *testing.T) {
	e := newEnv(t, llm.Scripted{Raw: `{"rules":[],"unsupported_criteria":["Paris veride yok"]}`})
	id, body := e.create("Paris'teki kullanıcılar")
	ae := body["audience_error"].(map[string]any)
	c := body["campaign"].(map[string]any)
	if ae["code"] != "unmappable" || !strings.Contains(ae["message"].(string), "Paris veride yok") || c["audience"] != nil || c["brief"] != "Paris'teki kullanıcılar" {
		t.Fatalf("body = %v", body)
	}
	if status, _, errBody := e.stream(id, map[string]any{}); status != http.StatusConflict || errCode(errBody) != "no_audience" {
		t.Fatalf("messages without audience: %d %v", status, errBody)
	}
}

func TestLLMUnavailableKeepsState(t *testing.T) {
	busy := llm.Scripted{Err: &llm.TransportError{StatusCode: 529, Err: io.EOF}}
	e := newEnv(t, llm.Scripted{Raw: audienceOK}, busy, busy, busy)
	id, _ := e.create("İstanbul'da son 30 günde sepete ekleyenler")
	status, body := e.do("POST", path(id, "/audience"), map[string]string{"brief": "yeni brief"})
	if status != 200 || errCode(map[string]any{"error": body["audience_error"]}) != "llm_unavailable" {
		t.Fatalf("rerun: %d %v", status, body)
	}
	c := body["campaign"].(map[string]any)
	if c["brief"] != "İstanbul'da son 30 günde sepete ekleyenler" || c["audience"] == nil {
		t.Errorf("a transient failure must not change the campaign: %v", c)
	}
}

func TestInvalidDraftIsStored(t *testing.T) {
	e := newEnv(t, llm.Scripted{Raw: audienceOK},
		llm.Scripted{Raw: tooLongMessages}, llm.Scripted{Raw: tooLongMessages}, llm.Scripted{Raw: tooLongMessages})
	id, _ := e.create("brief")
	_, evs, _ := e.stream(id, map[string]any{})
	if got := names(evs); strings.Join(got, ",") != "retry,retry,invalid" {
		t.Fatalf("events = %v", got)
	}
	c := e.get(id)
	errs, _ := c["content_errors"].([]any)
	if c["status"] != "draft" || c["contents"] == nil || len(errs) != 1 || errs[0].(map[string]any)["code"] != "too_long" ||
		errs[0].(map[string]any)["path"] != "tr.sms" {
		t.Fatalf("campaign = %v", c)
	}
	calls := e.llmCalls(id)
	if len(calls) != 4 || calls[1]["invalid"] != true || calls[3]["repair"] != 2 {
		t.Errorf("llm_calls = %v", calls)
	}
}

func TestNewAudienceResetsDraft(t *testing.T) {
	e := newEnv(t)
	id, _ := e.create(llm.ExampleBriefTR)
	e.stream(id, map[string]any{})
	if c := e.get(id); c["status"] != "ready" {
		t.Fatalf("status = %v", c["status"])
	}
	status, body := e.do("POST", path(id, "/audience"), map[string]string{"brief": llm.ExampleBriefEN})
	c := body["campaign"].(map[string]any)
	if status != 200 || c["status"] != "draft" || c["contents"] != nil || c["brief"] != llm.ExampleBriefEN {
		t.Fatalf("after new audience: %d %v", status, c)
	}
}

func TestGuidedRegeneration(t *testing.T) {
	revisedEN := `{"sms":"Dear {{first_name}}, your cart is waiting.","email_subject":"Your cart","email_body":"Dear {{first_name}},\n\nYour cart is waiting."}`
	e := newEnv(t, llm.Scripted{Raw: audienceOK}, llm.Scripted{Raw: validMessages}, llm.Scripted{Raw: `{"en":` + revisedEN + `}`})
	id, _ := e.create("brief")
	if status, _, errBody := e.stream(id, map[string]string{"feedback": "daha resmi"}); status != 422 || errCode(errBody) != "no_draft" {
		t.Fatalf("feedback without draft: %d %v", status, errBody)
	}
	e.stream(id, map[string]any{})
	before := e.get(id)["contents"].(map[string]any)

	// Revise English only.
	status, evs, _ := e.stream(id, map[string]any{"feedback": "More formal", "languages": []string{"en"}})
	if status != 200 || names(evs)[len(names(evs))-1] != "validated" {
		t.Fatalf("revise: %d %v", status, names(evs))
	}
	calls := e.mock.Calls()
	req := calls[len(calls)-1]
	sent := req.Messages[0].Content
	if !strings.Contains(sent, "More formal") || !strings.Contains(sent, "<previous_draft>") || !strings.Contains(sent, "<other_languages>") ||
		string(req.Schema) != string(prompt.MessagesSchema([]string{"en"})) {
		t.Errorf("regeneration request:\n%s\nschema %s", sent, req.Schema)
	}
	c := e.get(id)
	after := c["contents"].(map[string]any)
	if fmt.Sprint(after["tr"]) != fmt.Sprint(before["tr"]) || after["en"].(map[string]any)["email_subject"] != "Your cart" || c["status"] != "ready" {
		t.Errorf("after revising English: %v", c)
	}

	for _, tc := range []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"feedback": strings.Repeat("a", 501)}, "invalid_feedback"},
		{map[string]any{"languages": []string{"de"}}, "invalid_language"},
	} {
		if status, _, errBody := e.stream(id, tc.body); status != 422 || errCode(errBody) != tc.code {
			t.Errorf("%v: %d %v", tc.body, status, errBody)
		}
	}
}

// An audience with recipients in one language gets messages in that language only.
func TestOneLanguageAudience(t *testing.T) {
	e := newEnv(t, llm.Scripted{Raw: `{"rules":[{"field":"language","op":"eq","value":"en"}],"unsupported_criteria":[]}`},
		llm.Scripted{Raw: `{"en":` + validEN + `}`})
	id, body := e.create("English speakers")
	a := body["campaign"].(map[string]any)["audience"].(map[string]any)
	if fmt.Sprint(a["content_languages"]) != "[en]" {
		t.Fatalf("languages = %v", a["content_languages"])
	}
	status, evs, _ := e.stream(id, map[string]any{})
	if status != 200 || names(evs)[len(names(evs))-1] != "validated" {
		t.Fatalf("stream: %d %v", status, names(evs))
	}
	if req := e.mock.Calls()[1]; string(req.Schema) != string(prompt.MessagesSchema([]string{"en"})) {
		t.Errorf("schema = %s", req.Schema)
	}
	if c := e.get(id); c["status"] != "ready" || len(c["contents"].(map[string]any)) != 1 {
		t.Errorf("campaign = %v", c)
	}
}

func TestEmptyAudience(t *testing.T) {
	// Nobody's last cart add was exactly 365 days ago in the seed.
	e := newEnv(t, llm.Scripted{Raw: `{"rules":[{"field":"cart_added_days_ago","op":"gte","value":365}],"unsupported_criteria":[]}`})
	id, body := e.create("1 yıldır sepeti bekleyenler")
	if r := body["campaign"].(map[string]any)["audience"].(map[string]any)["reach"].(map[string]any); r["total"].(float64) != 0 {
		t.Fatalf("reach = %v", r)
	}
	if status, _, errBody := e.stream(id, map[string]any{}); status != 422 || errCode(errBody) != "empty_audience" {
		t.Fatalf("%d %v", status, errBody)
	}
}

func TestApprovedCampaignIsFrozen(t *testing.T) {
	e := newEnv(t)
	id, _ := e.create(llm.ExampleBriefTR)
	e.pool.Exec(context.Background(), `UPDATE campaigns SET status = 'approved' WHERE id = $1`, id)

	type check struct {
		name   string
		status int
		code   string
	}
	var checks []check
	s, b := e.do("POST", path(id, "/audience"), map[string]string{"brief": "x"})
	checks = append(checks, check{"audience", s, errCode(b)})
	s, b = e.do("POST", path(id, "/acknowledge"), nil)
	checks = append(checks, check{"acknowledge", s, errCode(b)})
	s, _, b = e.stream(id, map[string]any{})
	checks = append(checks, check{"messages", s, errCode(b)})
	for _, c := range checks {
		if c.status != http.StatusConflict || c.code != "not_editable" {
			t.Errorf("%s: %d %s", c.name, c.status, c.code)
		}
	}
}

// A second generation while one is running is refused; disconnecting cancels the
// running one, stores nothing and frees the campaign.
func TestBusyAndDisconnect(t *testing.T) {
	e := newEnv(t)
	e.mock.ChunkRunes, e.mock.ChunkDelay = 1, 20*time.Millisecond // a few seconds per answer
	id, _ := e.create(llm.ExampleBriefTR)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+path(id, "/messages"), strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if line != "event: delta\n" {
		t.Fatalf("first line = %q", line)
	}

	if status, _, errBody := e.stream(id, map[string]any{}); status != http.StatusConflict || errCode(errBody) != "busy" {
		t.Fatalf("second generation: %d %v", status, errBody)
	}
	if s, b := e.do("POST", path(id, "/audience"), map[string]string{"brief": "x"}); s != http.StatusConflict || errCode(b) != "busy" {
		t.Fatalf("audience during generation: %d %v", s, b)
	}

	cancel()
	resp.Body.Close()
	// The lock is released once the handler notices the disconnect.
	deadline := time.Now().Add(3 * time.Second)
	for {
		s, _ := e.do("POST", path(id, "/audience"), map[string]string{"brief": llm.ExampleBriefTR})
		if s == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("campaign still locked after disconnect: %d", s)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The cancelled generation stored nothing (the audience rerun above would have
	// cleared a draft too, so check the call log: no stored messages answer completed).
	var completed int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM llm_calls WHERE campaign_id = $1 AND step = 'messages' AND raw_response IS NOT NULL`, id).Scan(&completed)
	if completed != 0 {
		t.Errorf("%d messages answers completed after the disconnect", completed)
	}
	if c := e.get(id); c["contents"] != nil {
		t.Errorf("content stored after disconnect: %v", c["contents"])
	}
}

func TestRequestValidation(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name   string
		method string
		path   string
		body   any
		status int
		code   string
	}{
		{"empty brief", "POST", "/api/campaigns", map[string]string{"brief": "   "}, 422, "invalid_brief"},
		{"long brief", "POST", "/api/campaigns", map[string]string{"brief": strings.Repeat("ş", 2001)}, 422, "invalid_brief"},
		{"unknown field", "POST", "/api/campaigns", map[string]string{"brief": "x", "segment": "y"}, 400, "bad_request"},
		{"bad id", "GET", "/api/campaigns/abc", nil, 404, "not_found"},
		{"missing", "GET", "/api/campaigns/999", nil, 404, "not_found"},
	}
	for _, c := range cases {
		status, body := e.do(c.method, c.path, c.body)
		if status != c.status || errCode(body) != c.code {
			t.Errorf("%s: %d %v", c.name, status, body)
		}
	}
	if status, body := e.do("GET", "/healthz", nil); status != 200 || body["db"] != "ok" {
		t.Errorf("healthz: %d %v", status, body)
	}
}
