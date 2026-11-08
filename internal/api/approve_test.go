package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
)

// readyCampaign creates the example campaign and generates its messages (fixture mock).
func readyCampaign(t *testing.T, e *env) (int64, map[string]any) {
	t.Helper()
	id, _ := e.create(llm.ExampleBriefTR)
	if _, evs, _ := e.stream(id, map[string]any{}); names(evs)[len(names(evs))-1] != "validated" {
		t.Fatalf("generation: %v", names(evs))
	}
	c := e.get(id)
	return id, c["content"].(map[string]any)
}

func (e *env) approve(id int64, content any) (int, map[string]any) {
	e.t.Helper()
	return e.do("POST", path(id, "/approve"), map[string]any{"content": content})
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestApproveQueuesMessages(t *testing.T) {
	e := newEnv(t)
	id, content := readyCampaign(t, e)
	reach := e.get(id)["audience"].(map[string]any)["reach"].(map[string]any)

	status, body := e.approve(id, content)
	if status != 200 {
		t.Fatalf("approve: %d %v", status, body)
	}
	c := body["campaign"].(map[string]any)
	q := body["queued"].(map[string]any)
	if c["status"] != "approved" || c["approved_at"] == nil {
		t.Fatalf("campaign = %v", c)
	}
	// Queued exactly the reach shown in the preview: same segment, same as_of, same consent filter.
	if q["sms"] != reach["sms"] || q["email"] != reach["email"] {
		t.Fatalf("queued %v, reach %v", q, reach)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE campaign_id = $1 AND channel = 'sms'`, id); float64(n) != q["sms"] {
		t.Errorf("sms rows = %d, queued %v", n, q["sms"])
	}

	// Every row is rendered, pending, and has the right address and subject shape.
	for _, check := range []struct{ name, sql string }{
		{"unrendered placeholder", `SELECT count(*) FROM messages WHERE campaign_id = $1 AND (body LIKE '%{{%' OR subject LIKE '%{{%')`},
		{"not pending", `SELECT count(*) FROM messages WHERE campaign_id = $1 AND status <> 'pending'`},
		{"sms with subject", `SELECT count(*) FROM messages WHERE campaign_id = $1 AND channel = 'sms' AND subject IS NOT NULL`},
		{"email without subject", `SELECT count(*) FROM messages WHERE campaign_id = $1 AND channel = 'email' AND subject IS NULL`},
		{"wrong address", `SELECT count(*) FROM messages m JOIN users u ON u.id = m.user_id WHERE m.campaign_id = $1
			AND m.to_address IS DISTINCT FROM CASE m.channel WHEN 'sms' THEN u.phone ELSE u.email END`},
		{"no consent", `SELECT count(*) FROM messages m JOIN users u ON u.id = m.user_id WHERE m.campaign_id = $1
			AND ((m.channel = 'sms' AND NOT u.sms_consent) OR (m.channel = 'email' AND NOT u.email_consent))`},
	} {
		if n := e.count(check.sql, id); n != 0 {
			t.Errorf("%s: %d rows", check.name, n)
		}
	}

	// Recipients with an empty first name get "Merhaba,"; they are the only ones counted
	// as without name (the longest name was validated, so nobody falls back for length).
	emptyName := e.count(`SELECT count(*) FROM messages m JOIN users u ON u.id = m.user_id
		WHERE m.campaign_id = $1 AND m.channel = 'sms' AND u.first_name = ''`, id)
	if emptyName == 0 {
		t.Fatal("seed has an empty-name user in this segment; expected their SMS")
	}
	if n := e.count(`SELECT count(*) FROM messages m JOIN users u ON u.id = m.user_id
		WHERE m.campaign_id = $1 AND u.first_name = '' AND m.body NOT LIKE 'Merhaba,%'`, id); n != 0 {
		t.Errorf("%d empty-name messages don't start with \"Merhaba,\"", n)
	}
	if wn := q["without_name"].(map[string]any); wn["sms"] != float64(emptyName) {
		t.Errorf("without_name = %v, empty-name sms rows = %d", wn, emptyName)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE campaign_id = $1 AND body LIKE 'Merhaba Abdurrahmangazi,%'`, id); n == 0 {
		t.Error("the longest name must be used in full")
	}
}

func TestApproveIsIdempotent(t *testing.T) {
	e := newEnv(t)
	id, content := readyCampaign(t, e)

	var wg sync.WaitGroup
	statuses := make([]int, 5)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _ = e.approve(id, content)
		}()
	}
	wg.Wait()
	ok := 0
	for _, s := range statuses {
		switch s {
		case 200:
			ok++
		case http.StatusConflict:
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	if ok != 1 {
		t.Fatalf("%d approvals succeeded, want exactly 1 (%v)", ok, statuses)
	}
	before := e.count(`SELECT count(*) FROM messages WHERE campaign_id = $1`, id)
	if dups := e.count(`SELECT count(*) - count(DISTINCT (user_id, channel)) FROM messages WHERE campaign_id = $1`, id); dups != 0 {
		t.Errorf("%d duplicate messages", dups)
	}

	// A later approve, even with different content, changes nothing.
	edited := map[string]any{"sms": "Başka metin", "email_subject": "Başka", "email_body": "Başka"}
	status, body := e.approve(id, edited)
	if status != http.StatusConflict || errCode(body) != "already_approved" {
		t.Fatalf("second approve: %d %v", status, body)
	}
	if after := e.count(`SELECT count(*) FROM messages WHERE campaign_id = $1`, id); after != before {
		t.Errorf("messages %d → %d", before, after)
	}
	if c := e.get(id); c["content"].(map[string]any)["sms"] != content["sms"] {
		t.Error("approved content was overwritten")
	}
}

func TestApproveRejectsInvalidContent(t *testing.T) {
	e := newEnv(t)
	id, content := readyCampaign(t, e)
	bad := map[string]any{"sms": content["sms"].(string) + strings.Repeat(" uzun", 30), "email_subject": "{name}", "email_body": "x"}
	status, body := e.approve(id, bad)
	errs := body["error"].(map[string]any)["errors"].([]any)
	if status != 422 || errCode(body) != "invalid_content" || len(errs) != 2 {
		t.Fatalf("%d %v", status, body)
	}
	if e.get(id)["status"] != "ready" || e.count(`SELECT count(*) FROM messages WHERE campaign_id = $1`, id) != 0 {
		t.Error("a rejected approve must change nothing")
	}
}

// A draft that ran out of repairs is fixed by hand and approved from status draft; a
// campaign without any generated draft can be approved with hand-written content too.
func TestApproveHandWrittenContent(t *testing.T) {
	e := newEnv(t, llm.Scripted{Raw: audienceOK},
		llm.Scripted{Raw: tooLongMessages}, llm.Scripted{Raw: tooLongMessages}, llm.Scripted{Raw: tooLongMessages},
		llm.Scripted{Raw: audienceOK})
	fixed := map[string]any{"sms": "Merhaba {{first_name}}, sepetin bekliyor!", "email_subject": "Sepetin", "email_body": "Merhaba {{first_name}},"}

	id, _ := e.create("brief")
	e.stream(id, map[string]any{})
	if c := e.get(id); c["status"] != "draft" {
		t.Fatalf("status = %v", c["status"])
	}
	if status, body := e.approve(id, fixed); status != 200 {
		t.Fatalf("approve fixed draft: %d %v", status, body)
	}

	id2, _ := e.create("brief")
	if status, body := e.approve(id2, fixed); status != 200 {
		t.Fatalf("approve without draft: %d %v", status, body)
	}
}

func TestApprovePreconditions(t *testing.T) {
	e := newEnv(t, llm.Scripted{Raw: `{"rules":[{"field":"city","op":"eq","value":"Ankara"}],"unsupported_criteria":["30 yaş üstü"]}`})
	id, _ := e.create("30 yaş üstü Ankaralılar")
	valid := map[string]any{"sms": "Merhaba!", "email_subject": "Konu", "email_body": "Gövde"}
	if status, body := e.approve(id, valid); status != 409 || errCode(body) != "unsupported_not_acknowledged" {
		t.Fatalf("unacknowledged: %d %v", status, body)
	}
	e.do("POST", path(id, "/acknowledge"), nil)

	// Nobody reachable any more: rejected, nothing changes.
	e.pool.Exec(context.Background(), `UPDATE users SET sms_consent = false, email_consent = false WHERE city = 'Ankara'`)
	if status, body := e.approve(id, valid); status != 422 || errCode(body) != "empty_audience" {
		t.Fatalf("no recipients: %d %v", status, body)
	}
	if c := e.get(id); c["status"] != "draft" {
		t.Errorf("status = %v after a rejected approve", c["status"])
	}
}

// An audience counted on an earlier day must be refreshed before approval. Refreshing
// recounts for today without calling Claude and keeps the draft and acknowledgement.
func TestStaleAudience(t *testing.T) {
	e := newEnv(t)
	id, content := readyCampaign(t, e)
	e.pool.Exec(context.Background(), `UPDATE campaigns SET audience_as_of = now() - interval '25 hours' WHERE id = $1`, id)

	if status, body := e.approve(id, content); status != 409 || errCode(body) != "stale_audience" {
		t.Fatalf("stale approve: %d %v", status, body)
	}
	calls := len(e.mock.Calls())
	status, c := e.do("POST", path(id, "/audience/refresh"), nil)
	if status != 200 || c["status"] != "ready" || c["content"] == nil {
		t.Fatalf("refresh: %d %v", status, c)
	}
	if len(e.mock.Calls()) != calls {
		t.Error("refresh must not call the model")
	}
	if status, body := e.approve(id, content); status != 200 {
		t.Fatalf("approve after refresh: %d %v", status, body)
	}
}

// If the longest name in the audience grew, refresh re-validates the draft.
func TestRefreshRevalidatesDraft(t *testing.T) {
	e := newEnv(t)
	id, _ := readyCampaign(t, e)
	e.pool.Exec(context.Background(), `UPDATE users SET first_name = repeat('Ü', 70) WHERE first_name = 'Selin' AND last_name = 'Aydın'`)

	status, c := e.do("POST", path(id, "/audience/refresh"), nil)
	if status != 200 {
		t.Fatalf("refresh: %d %v", status, c)
	}
	errs, _ := c["content_errors"].([]any)
	reach := c["audience"].(map[string]any)["reach"].(map[string]any)
	if c["status"] != "draft" || len(errs) == 0 || reach["longest_first_name"].(float64) != 70 {
		t.Fatalf("after refresh: status %v, errors %v, reach %v", c["status"], errs, reach)
	}
}
