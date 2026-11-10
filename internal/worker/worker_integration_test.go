package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denizsahiner/smart-campaign-composer/internal/dbtest"
	"github.com/denizsahiner/smart-campaign-composer/internal/provider"
)

// queueCampaign inserts an approved campaign with n SMS and n email messages, plus one
// invalid recipient per channel, and returns its id.
func queueCampaign(t *testing.T, pool *pgxpool.Pool, n int) int64 {
	t.Helper()
	ctx := context.Background()
	var cid int64
	if err := pool.QueryRow(ctx, `INSERT INTO campaigns (brief, status, approved_at)
		VALUES ('test', 'approved', now()) RETURNING id`).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	for i := range n + 1 {
		var uid int64
		if err := pool.QueryRow(ctx, `INSERT INTO users (first_name, last_name, city, language)
			VALUES ('Ayşe', 'Yılmaz', 'İstanbul', 'tr') RETURNING id`).Scan(&uid); err != nil {
			t.Fatal(err)
		}
		phone, email := fmt.Sprintf("+90532%07d", i), fmt.Sprintf("u%d@example.com", i)
		if i == n { // the permanent failures
			phone, email = "+902125550123", "x@bounce.invalid"
		}
		if _, err := pool.Exec(ctx, `INSERT INTO messages (campaign_id, user_id, channel, to_address, subject, body)
			VALUES ($1, $2, 'sms', $3, NULL, 'Merhaba Ayşe'), ($1, $2, 'email', $4, 'Konu', 'Merhaba Ayşe')`,
			cid, uid, phone, email); err != nil {
			t.Fatal(err)
		}
	}
	return cid
}

func testConfig() Config {
	return Config{
		RatePerChannel: 500, Concurrency: 16, BatchSize: 8, Lease: 5 * time.Second,
		MaxAttempts: 8, BackoffBase: 10 * time.Millisecond, PollInterval: 20 * time.Millisecond,
	}
}

// runUntilDone runs the worker until nothing is pending, sending or scheduled.
func runUntilDone(t *testing.T, pool *pgxpool.Pool, w *Worker, limit time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	deadline := time.Now().Add(limit)
	for {
		var open int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM messages
			WHERE status IN ('pending', 'sending', 'retry_scheduled')`).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("%d messages still open after %v", open, limit)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}

type row struct {
	id                int64
	channel, status   string
	attempts          int
	providerMessageID *string
}

func rows(t *testing.T, pool *pgxpool.Pool) map[string]row {
	t.Helper()
	rs, err := pool.Query(context.Background(), `SELECT id, channel, status, attempts, provider_message_id FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]row{}
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.id, &r.channel, &r.status, &r.attempts, &r.providerMessageID); err != nil {
			t.Fatal(err)
		}
		out[strconv.FormatInt(r.id, 10)] = r
	}
	return out
}

// End to end against a provider that fails a lot: every message ends sent or failed,
// nobody gets a message twice, and the DB agrees with what the provider delivered.
func TestEndToEndUnreliableProvider(t *testing.T) {
	pool := dbtest.New(t)
	cid := queueCampaign(t, pool, 150)

	p := provider.New(provider.Config{
		ErrorRate: 0.2, TimeoutRate: 0.05,
		ThrottleEvery: time.Second, ThrottleFor: 200 * time.Millisecond,
		LatencyMax: 5 * time.Millisecond, Seed: 1,
	})
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	w := New(testConfig(), NewQueue(pool), NewHTTPSender(ts.URL, 150*time.Millisecond))
	runUntilDone(t, pool, w, 60*time.Second)

	snap := p.Snapshot()
	db := rows(t, pool)
	sent := 0
	for id, r := range db {
		switch r.status {
		case "sent":
			sent++
		case "failed":
		default:
			t.Fatalf("message %s ended %q", id, r.status)
		}
	}
	for _, d := range snap.Deliveries {
		r, ok := db[d.MessageID]
		if !ok || r.status != "sent" || r.providerMessageID == nil || *r.providerMessageID != d.ProviderMessageID {
			t.Errorf("delivery %+v does not match row %+v", d, r)
		}
	}
	// Deliveries are keyed by message_id, so one entry per id; equal counts mean no row
	// was marked sent without a delivery and no delivery is missing a sent row.
	if len(snap.Deliveries) != sent {
		t.Errorf("%d deliveries, %d sent rows", len(snap.Deliveries), sent)
	}

	// Invalid recipients fail on their first attempt; nothing else fails at this rate.
	var failed []row
	for _, r := range db {
		if r.status == "failed" {
			failed = append(failed, r)
		}
	}
	if len(failed) != 2 || failed[0].attempts != 1 || failed[1].attempts != 1 {
		t.Errorf("failed rows %+v, want the two invalid recipients after one attempt", failed)
	}
	if n := countWhere(t, pool, `failure = 'permanent' AND to_address IN ('+902125550123', 'x@bounce.invalid')`); n != 2 {
		t.Errorf("%d invalid recipients marked permanent, want 2", n)
	}

	sms := snap.Totals["sms"]
	if sms.Rejected["server_error"] == 0 || sms.Rejected["throttled"] == 0 || sms.HungAfterDelivery+snap.Totals["email"].HungAfterDelivery == 0 {
		t.Errorf("faults not exercised: %+v %+v", sms, snap.Totals["email"])
	}

	var status string
	pool.QueryRow(context.Background(), `SELECT status FROM campaigns WHERE id = $1`, cid).Scan(&status)
	if status != "completed" {
		t.Errorf("campaign status %q, want completed", status)
	}
}

// A provider limit below the worker's rate produces real rate-limit 429s; they pause the
// channel and are refunded, so every message is sent on attempt 1.
func TestRateLimit429sAreRefunded(t *testing.T) {
	pool := dbtest.New(t)
	queueCampaign(t, pool, 40)
	p := provider.New(provider.Config{RatePerChannel: 20, Seed: 1})
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	w := New(testConfig(), NewQueue(pool), NewHTTPSender(ts.URL, time.Second))
	runUntilDone(t, pool, w, 30*time.Second)

	if p.Snapshot().Totals["sms"].Rejected[provider.ErrRateLimited] == 0 {
		t.Fatal("provider never rate-limited: the test exercises nothing")
	}
	for id, r := range rows(t, pool) {
		if r.status == "sent" && r.attempts != 1 {
			t.Errorf("message %s sent after %d counted attempts; 429s must not count", id, r.attempts)
		}
	}
}

func TestLeaseExpiryAndFencedWrite(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	queueCampaign(t, pool, 1)
	q := NewQueue(pool)

	first, err := q.Claim(ctx, "sms", 1, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim: %v %v", first, err)
	}
	// Still leased: a second claimer must not get it.
	if again, _ := q.Claim(ctx, "sms", 2, time.Minute); len(again) != 1 || again[0].ID == first[0].ID {
		t.Fatalf("leased row claimed twice: %+v", again)
	}
	// The first worker "dies": its lease runs out and the row is claimed again.
	pool.Exec(ctx, `UPDATE messages SET locked_until = now() - interval '1 second' WHERE id = $1`, first[0].ID)
	second, err := q.Claim(ctx, "sms", 1, time.Minute)
	if err != nil || len(second) != 1 || second[0].ID != first[0].ID || second[0].Attempts != 2 {
		t.Fatalf("expired lease not reclaimed: %+v %v", second, err)
	}

	// The late result of the first attempt is dropped; the second attempt's is written.
	if ok, err := q.Reschedule(ctx, first[0], time.Second, "late 500", false); ok || err != nil {
		t.Fatalf("stale write accepted: %v %v", ok, err)
	}
	if ok, err := q.MarkSent(ctx, second[0], "sms_x"); !ok || err != nil {
		t.Fatalf("current write rejected: %v %v", ok, err)
	}
	if ok, _ := q.MarkFailed(ctx, first[0], "late", FailurePermanent); ok {
		t.Fatal("stale write overwrote a sent row")
	}
}

// An unreachable provider refunds the attempt and pauses the channel; the rows the paused
// channel had claimed are handed back instead of waiting out the pause.
func TestUnreachableProviderRefundsAndPauses(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	queueCampaign(t, pool, 2)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()

	q := NewQueue(pool)
	w := New(testConfig(), q, NewHTTPSender("http://"+addr, time.Second))
	ch := w.channels[0]
	msgs, err := q.Claim(ctx, "sms", 2, time.Minute)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("claim: %v %v", msgs, err)
	}
	w.process(ctx, ch, msgs[0])
	if left := ch.pauseRemaining(); left <= 0 || left > time.Second {
		t.Fatalf("pause %v after first refusal, want ~1s", left)
	}
	w.process(ctx, ch, msgs[1]) // channel paused: handed back without a send

	var attempts []int
	rs, _ := pool.Query(ctx, `SELECT attempts FROM messages WHERE id = ANY($1) AND status = 'retry_scheduled'`,
		[]int64{msgs[0].ID, msgs[1].ID})
	for rs.Next() {
		var a int
		rs.Scan(&a)
		attempts = append(attempts, a)
	}
	rs.Close()
	if len(attempts) != 2 || attempts[0] != 0 || attempts[1] != 0 {
		t.Fatalf("attempts after refusal %v, want both rescheduled with 0", attempts)
	}

	// Another refusal after the pause grows it to 2s.
	ch.mu.Lock()
	ch.pausedUntil = time.Time{}
	ch.mu.Unlock()
	if d, _ := ch.unreachable(); d != 2*time.Second {
		t.Fatalf("second pause %v, want 2s", d)
	}
}

func TestRetriesExhaustToFailed(t *testing.T) {
	pool := dbtest.New(t)
	queueCampaign(t, pool, 3)
	p := provider.New(provider.Config{ErrorRate: 1, Seed: 1})
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	cfg := testConfig()
	cfg.MaxAttempts = 3
	cfg.BackoffBase = time.Millisecond
	w := New(cfg, NewQueue(pool), NewHTTPSender(ts.URL, time.Second))
	runUntilDone(t, pool, w, 30*time.Second)
	for id, r := range rows(t, pool) {
		if r.status != "failed" {
			t.Errorf("message %s: %q, want failed", id, r.status)
		}
	}
	// The invalid recipients are rejected before the dice, so they are permanent.
	if n := countWhere(t, pool, `attempts = 3 AND last_error LIKE '5%' AND failure = 'retries_exhausted'`); n != 6 {
		t.Errorf("%d messages failed after 3 attempts with a 5xx, want 6", n)
	}
	if n := countWhere(t, pool, `failure = 'permanent'`); n != 2 {
		t.Errorf("%d permanent failures, want the 2 invalid recipients", n)
	}
}

func countWhere(t *testing.T, pool *pgxpool.Pool, cond string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM messages WHERE `+cond).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Each retry is a warning and each message given up on an error, both with the campaign
// and the message, so the logs page can show a campaign's delivery problems.
func TestSendOutcomesAreLogged(t *testing.T) {
	pool := dbtest.New(t)
	cid := queueCampaign(t, pool, 3) // plus one permanent failure per channel
	p := provider.New(provider.Config{ErrorRate: 1, Seed: 1})
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	cfg := testConfig()
	cfg.MaxAttempts = 2
	cfg.BackoffBase = time.Millisecond
	w := New(cfg, NewQueue(pool), NewHTTPSender(ts.URL, time.Second))
	runUntilDone(t, pool, w, 30*time.Second)

	count := map[string]int{}
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var l struct {
			Level      string `json:"level"`
			Msg        string `json:"msg"`
			Reason     string `json:"reason"`
			CampaignID int64  `json:"campaign_id"`
			MessageID  int64  `json:"message_id"`
		}
		json.Unmarshal(line, &l)
		if l.Msg != "send failed, retry scheduled" && l.Msg != "message failed" {
			continue
		}
		if l.CampaignID != cid || l.MessageID == 0 {
			t.Errorf("line without campaign or message: %s", line)
		}
		count[l.Level+" "+l.Msg+" "+l.Reason]++
	}
	// 3 good recipients per channel: 1 retry then given up. The bad one is rejected at once.
	want := map[string]int{
		"WARN send failed, retry scheduled ":        6,
		"ERROR message failed retries exhausted":    6,
		"ERROR message failed rejected by provider": 2,
	}
	if fmt.Sprint(count) != fmt.Sprint(want) {
		t.Errorf("logged %v, want %v", count, want)
	}
}
