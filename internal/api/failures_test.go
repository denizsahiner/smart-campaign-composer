package api_test

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// failCampaign approves the example campaign and fails three of its messages the way the
// worker would: an SMS and an email the provider rejected, and an email whose retries
// ran out. It returns the campaign and the failed message ids in that order.
func failCampaign(t *testing.T, e *env) (int64, [3]int64) {
	t.Helper()
	id := approvedCampaign(t, e)
	var ids [3]int64
	for i, f := range []struct{ channel, addr, lastErr, failure string }{
		{"sms", "+902125550123", "400 invalid_recipient: not a Turkish mobile number", "permanent"},
		{"email", "=HYPERLINK(\"x\")@bounce.invalid", "400 invalid_recipient: domain does not exist", "permanent"},
		{"email", "ok@example.com", "503 provider_error: try again", "retries_exhausted"},
	} {
		err := e.pool.QueryRow(context.Background(), `
			UPDATE messages SET status = 'failed', failure = $4, to_address = $3, last_error = $5, attempts = 1
			WHERE id = (SELECT min(id) FROM messages WHERE campaign_id = $1 AND channel = $2 AND status = 'pending')
			RETURNING id`, id, f.channel, f.addr, f.failure, f.lastErr).Scan(&ids[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	return id, ids
}

func TestListFailures(t *testing.T) {
	e := newEnv(t)
	id, ids := failCampaign(t, e)

	status, body := e.do("GET", path(id, "/failures"), nil)
	if status != 200 || body["total"] != float64(2) || body["next_before"] != nil {
		t.Fatalf("%d %v", status, body)
	}
	list := body["failures"].([]any)
	if len(list) != 2 {
		t.Fatalf("failures = %v, want only the two permanent ones", list)
	}
	email, sms := list[0].(map[string]any), list[1].(map[string]any) // newest first
	if sms["message_id"] != float64(ids[0]) || sms["reason"] != "Geçersiz telefon numarası" ||
		sms["address"] != "+902125550123" || sms["first_name"] == "" || sms["error"] != "400 invalid_recipient: not a Turkish mobile number" {
		t.Errorf("sms = %v", sms)
	}
	if email["message_id"] != float64(ids[1]) || email["reason"] != "Geçersiz e-posta adresi" || email["channel"] != "email" {
		t.Errorf("email = %v", email)
	}

	// Paging.
	_, body = e.do("GET", path(id, "/failures?limit=1"), nil)
	if len(body["failures"].([]any)) != 1 || body["next_before"] != float64(ids[1]) || body["total"] != float64(2) {
		t.Fatalf("page 1 = %v", body)
	}
	_, body = e.do("GET", path(id, fmt.Sprintf("/failures?limit=1&before=%d", ids[1])), nil)
	if got := body["failures"].([]any); len(got) != 1 || got[0].(map[string]any)["message_id"] != float64(ids[0]) || body["next_before"] != nil {
		t.Errorf("page 2 = %v", body)
	}

	for _, q := range []string{"?limit=0", "?limit=501", "?before=x"} {
		if status, body := e.do("GET", path(id, "/failures"+q), nil); status != 400 || errCode(body) != "bad_request" {
			t.Errorf("%s: %d %v", q, status, body)
		}
	}
	if status, _ := e.do("GET", "/api/campaigns/999999/failures", nil); status != 404 {
		t.Errorf("unknown campaign: %d", status)
	}
}

func TestListFailuresEmpty(t *testing.T) {
	e := newEnv(t)
	id := approvedCampaign(t, e)
	if status, body := e.do("GET", path(id, "/failures"), nil); status != 200 || body["total"] != float64(0) || len(body["failures"].([]any)) != 0 {
		t.Errorf("%d %v", status, body)
	}
}

func TestFailuresCSV(t *testing.T) {
	e := newEnv(t)
	id, _ := failCampaign(t, e)

	resp, err := http.Get(e.srv.URL + path(id, "/failures.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" ||
		resp.Header.Get("Content-Disposition") != fmt.Sprintf(`attachment; filename="kampanya-%d-gonderilemeyenler.csv"`, id) {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	text, ok := strings.CutPrefix(string(raw), "\uFEFF")
	if !ok {
		t.Fatal("no UTF-8 BOM: Excel would garble ş, ğ, ı")
	}
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = ';'
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0][0] != "Kullanıcı ID" || recs[0][5] != "Neden" {
		t.Fatalf("records = %q, want a header and the two permanent failures", recs)
	}
	sms, email := recs[1], recs[2] // oldest first
	// A leading + or = would be read as a formula; the ' keeps it text.
	if sms[3] != "SMS" || sms[4] != "'+902125550123" || sms[5] != "Geçersiz telefon numarası" {
		t.Errorf("sms row = %q", sms)
	}
	if email[3] != "E-posta" || email[4] != `'=HYPERLINK("x")@bounce.invalid` || !strings.HasPrefix(email[6], "400 invalid_recipient") {
		t.Errorf("email row = %q", email)
	}
}
