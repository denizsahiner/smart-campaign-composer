package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denizsahiner/smart-campaign-composer/internal/api"
	"github.com/denizsahiner/smart-campaign-composer/internal/provider"
)

func approvedCampaign(t *testing.T, e *env) int64 {
	t.Helper()
	id, content := readyCampaign(t, e)
	if status, body := e.approve(id, content); status != 200 {
		t.Fatalf("approve: %d %v", status, body)
	}
	return id
}

// liveEvents opens the event stream and delivers each event (comments skipped) on the
// returned channel, which closes with the stream.
func (e *env) liveEvents(ctx context.Context, id int64) (int, <-chan sseEvent) {
	e.t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path(id, "/events"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	out := make(chan sseEvent, 64)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		close(out)
		return resp.StatusCode, out
	}
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				cur.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data)
			case line == "" && cur.name != "":
				out <- cur
				cur = sseEvent{}
			}
		}
	}()
	return resp.StatusCode, out
}

func next(t *testing.T, evs <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-evs:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
	}
	return sseEvent{}
}

func channelCounts(ev sseEvent, ch string) map[string]any {
	return ev.data["channels"].(map[string]any)[ch].(map[string]any)
}

// The stream starts with the current counts, follows the worker's progress, and ends
// with done once the campaign is completed.
func TestCampaignEvents(t *testing.T) {
	e := newEnv(t)
	id := approvedCampaign(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	status, evs := e.liveEvents(ctx, id)
	if status != http.StatusOK {
		t.Fatalf("events: %d", status)
	}
	first := next(t, evs)
	if first.name != "progress" || first.data["status"] != "approved" {
		t.Fatalf("first event %v", first)
	}
	sms := channelCounts(first, "sms")
	total := first.data["total"].(map[string]any)["sms"].(float64)
	if sms["pending"].(float64) != total || sms["sent"].(float64) != 0 || sms["skipped"] == nil {
		t.Fatalf("initial sms counts %v (total %v)", sms, total)
	}

	// "The worker" sends some SMS: a new progress event with the new counts.
	if _, err := e.pool.Exec(ctx, `UPDATE messages SET status = 'sent', sent_at = now()
		WHERE id IN (SELECT id FROM messages WHERE campaign_id = $1 AND channel = 'sms' LIMIT 5)`, id); err != nil {
		t.Fatal(err)
	}
	ev := next(t, evs)
	if ev.name != "progress" || channelCounts(ev, "sms")["sent"].(float64) != 5 {
		t.Fatalf("after 5 sends: %v", ev)
	}
	if rate := ev.data["rate_per_second"].(map[string]any)["sms"].(float64); rate != 1 {
		t.Errorf("sms rate %v, want 5 sends / 5 s = 1", rate)
	}

	// Everything ends and the worker marks the campaign completed.
	if _, err := e.pool.Exec(ctx, `UPDATE messages SET status = 'sent', sent_at = now() WHERE campaign_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE campaigns SET status = 'completed' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var last sseEvent
	for ev := range evs {
		last = ev
	}
	if last.name != "done" || last.data["status"] != "completed" || channelCounts(last, "email")["pending"].(float64) != 0 {
		t.Fatalf("last event %v, want done with everything sent", last)
	}
}

// A completed campaign can still be opened: one progress, done, closed.
func TestCampaignEventsCompleted(t *testing.T) {
	e := newEnv(t)
	id := approvedCampaign(t, e)
	ctx := context.Background()
	e.pool.Exec(ctx, `UPDATE messages SET status = 'sent', sent_at = now() WHERE campaign_id = $1`, id)
	e.pool.Exec(ctx, `UPDATE campaigns SET status = 'completed' WHERE id = $1`, id)

	_, evs := e.liveEvents(ctx, id)
	var got []string
	for ev := range evs {
		got = append(got, ev.name)
	}
	if strings.Join(got, ",") != "progress,done" {
		t.Fatalf("events %v, want progress,done", got)
	}
}

func TestCampaignEventsNotApproved(t *testing.T) {
	e := newEnv(t)
	id, _ := readyCampaign(t, e)
	status, body := e.do("GET", path(id, "/events"), nil)
	if status != http.StatusConflict || errCode(body) != "not_approved" {
		t.Fatalf("draft campaign: %d %v", status, body)
	}
	if status, _ := e.do("GET", "/api/campaigns/999999/events", nil); status != http.StatusNotFound {
		t.Fatalf("missing campaign: %d", status)
	}
}

func TestCampaignEventsPing(t *testing.T) {
	e := newEnv(t)
	id := approvedCampaign(t, e)
	m := api.DefaultMonitoring()
	m.PollInterval = time.Hour
	m.PingInterval = 20 * time.Millisecond
	e.server.WithMonitoring(m)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path(id, "/events"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == ": ping" {
			return
		}
	}
	t.Fatal("no keep-alive ping on an idle stream")
}

func TestMetrics(t *testing.T) {
	e := newEnv(t)
	id := approvedCampaign(t, e)
	ctx := context.Background()
	e.pool.Exec(ctx, `UPDATE messages SET status = 'sent' WHERE id IN
		(SELECT id FROM messages WHERE campaign_id = $1 AND channel = 'email' LIMIT 3)`, id)

	prov := provider.New(provider.Config{Seed: 1})
	provSrv := httptest.NewServer(prov.Handler())
	defer provSrv.Close()
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"status":"unavailable","error":"channel loop stalled: [sms]"}`))
	}))
	defer worker.Close()
	m := api.DefaultMonitoring()
	m.WorkerHealthURL, m.ProviderURL = worker.URL+"/healthz", provSrv.URL
	e.server.WithMonitoring(m)

	status, body := e.do("GET", "/api/metrics", nil)
	if status != 200 {
		t.Fatalf("metrics: %d %v", status, body)
	}
	smsTotal := float64(e.count(`SELECT count(*) FROM messages WHERE channel = 'sms'`))
	emailTotal := float64(e.count(`SELECT count(*) FROM messages WHERE channel = 'email'`))
	depth := body["queue_depth"].(map[string]any)
	if depth["sms"] != smsTotal || depth["email"] != emailTotal-3 {
		t.Errorf("queue_depth %v, want sms %v email %v", depth, smsTotal, emailTotal-3)
	}
	msgs := body["messages"].(map[string]any)["email"].(map[string]any)
	if msgs["sent"] != 3.0 || msgs["failed"] != 0.0 {
		t.Errorf("email counts %v", msgs)
	}
	llm := body["llm"].(map[string]any)
	if llm["calls"].(float64) < 2 || llm["output_tokens"] == nil {
		t.Errorf("llm %v, want the audience and messages calls", llm)
	}
	w := body["worker"].(map[string]any)
	if w["status"] != "unavailable" || w["error"] != "channel loop stalled: [sms]" {
		t.Errorf("worker %v", w)
	}
	p := body["provider"].(map[string]any)
	if p["status"] != "ok" || p["totals"].(map[string]any)["sms"] == nil {
		t.Errorf("provider %v", p)
	}

	// A dependency that doesn't answer doesn't break the endpoint.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String()
	l.Close()
	m.WorkerHealthURL, m.ProviderURL = dead+"/healthz", dead
	e.server.WithMonitoring(m)
	status, body = e.do("GET", "/api/metrics", nil)
	if status != 200 || body["worker"].(map[string]any)["status"] != "unreachable" ||
		body["provider"].(map[string]any)["status"] != "unreachable" {
		t.Fatalf("with dependencies down: %d worker=%v provider=%v", status, body["worker"], body["provider"])
	}
}
