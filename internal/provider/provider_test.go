package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// quiet is a provider with no injected failures and no limit.
func quiet() Config { return Config{} }

func start(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	cfg.Seed = 7
	s := New(cfg)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

type answer struct {
	status     int
	retryAfter string
	ok         SendResponse
	err        ErrorResponse
}

func post(t *testing.T, client *http.Client, url, channel string, req SendRequest) (answer, error) {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := client.Post(url+"/"+channel, "application/json", bytes.NewReader(body))
	if err != nil {
		return answer{}, err
	}
	defer resp.Body.Close()
	a := answer{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
	if resp.StatusCode == http.StatusOK {
		json.NewDecoder(resp.Body).Decode(&a.ok)
	} else {
		json.NewDecoder(resp.Body).Decode(&a.err)
	}
	return a, nil
}

func sms(id string) SendRequest {
	return SendRequest{MessageID: id, To: "+905321234567", Body: "Merhaba"}
}

func mustPost(t *testing.T, url, channel string, req SendRequest) answer {
	t.Helper()
	a, err := post(t, http.DefaultClient, url, channel, req)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestValidation(t *testing.T) {
	_, ts := start(t, quiet())
	email := func(to string) SendRequest {
		return SendRequest{MessageID: "e", To: to, Subject: "Konu", Body: "Merhaba"}
	}
	cases := []struct {
		name, channel string
		req           SendRequest
		code          string
	}{
		{"landline", "sms", SendRequest{MessageID: "1", To: "+902125550123", Body: "x"}, ErrInvalidRecipient},
		{"short number", "sms", SendRequest{MessageID: "1", To: "+90532123", Body: "x"}, ErrInvalidRecipient},
		{".invalid domain", "email", email("gizem.arslan@bounce.invalid"), ErrInvalidRecipient},
		{"no at sign", "email", email("gizem.example.com"), ErrInvalidRecipient},
		{"missing id", "sms", SendRequest{To: "+905321234567", Body: "x"}, ErrInvalidRequest},
		{"missing body", "sms", SendRequest{MessageID: "1", To: "+905321234567"}, ErrInvalidRequest},
		{"email without subject", "email", SendRequest{MessageID: "1", To: "a@example.com", Body: "x"}, ErrInvalidRequest},
		{"sms with subject", "sms", SendRequest{MessageID: "1", To: "+905321234567", Subject: "s", Body: "x"}, ErrInvalidRequest},
	}
	for _, c := range cases {
		a := mustPost(t, ts.URL, c.channel, c.req)
		if a.status != http.StatusBadRequest || a.err.Error != c.code {
			t.Errorf("%s: got %d %q, want 400 %q", c.name, a.status, a.err.Error, c.code)
		}
	}
	if a := mustPost(t, ts.URL, "email", email("ayse@example.com")); a.status != http.StatusOK {
		t.Errorf("valid email: got %d", a.status)
	}
}

func TestDuplicateReturnsOriginalAnswer(t *testing.T) {
	s, ts := start(t, quiet())
	first := mustPost(t, ts.URL, "sms", sms("42"))
	second := mustPost(t, ts.URL, "sms", sms("42"))
	if first.status != 200 || second.status != 200 {
		t.Fatalf("statuses %d, %d", first.status, second.status)
	}
	if first.ok.Duplicate || !second.ok.Duplicate || first.ok.ProviderMessageID != second.ok.ProviderMessageID {
		t.Fatalf("first %+v, second %+v", first.ok, second.ok)
	}
	snap := s.Snapshot()
	if len(snap.Deliveries) != 1 || snap.Deliveries[0].Requests != 2 || snap.Totals["sms"].Duplicates != 1 {
		t.Fatalf("snapshot %+v", snap)
	}
}

// A failure must not be cached: the next attempt with the same id is processed.
func TestFailuresAreNotCached(t *testing.T) {
	s, ts := start(t, Config{ErrorRate: 1})
	if a := mustPost(t, ts.URL, "sms", sms("7")); a.status < 500 {
		t.Fatalf("got %d, want 5xx", a.status)
	}
	s.mu.Lock()
	s.cfg.ErrorRate = 0
	s.mu.Unlock()
	a := mustPost(t, ts.URL, "sms", sms("7"))
	if a.status != 200 || a.ok.Duplicate {
		t.Fatalf("retry after 5xx: %d %+v, want a fresh delivery", a.status, a.ok)
	}
}

func TestInProgress(t *testing.T) {
	_, ts := start(t, Config{LatencyMin: 300 * time.Millisecond, LatencyMax: 300 * time.Millisecond})
	var wg sync.WaitGroup
	var first answer
	wg.Go(func() { first = mustPost(t, ts.URL, "sms", sms("9")) })
	time.Sleep(100 * time.Millisecond)
	second := mustPost(t, ts.URL, "sms", sms("9"))
	wg.Wait()
	if second.status != http.StatusConflict || second.err.Error != ErrInProgress {
		t.Fatalf("concurrent repeat: %d %+v, want 409 in_progress", second.status, second.err)
	}
	if first.status != 200 {
		t.Fatalf("first: %d", first.status)
	}
}

// Every injected timeout either sent nothing (the retry delivers) or delivered before
// hanging (the retry is a duplicate). Either way: exactly one delivery per id.
func TestTimeoutsNeverDuplicate(t *testing.T) {
	s, ts := start(t, Config{TimeoutRate: 1})
	client := &http.Client{Timeout: 100 * time.Millisecond}
	const n = 20
	for i := range n {
		if _, err := post(t, client, ts.URL, "sms", sms(fmt.Sprint(i))); err == nil {
			t.Fatalf("request %d did not time out", i)
		}
	}
	s.mu.Lock()
	s.cfg.TimeoutRate = 0
	s.mu.Unlock()
	// Wait until hung handlers noticed the disconnect and released their ids.
	time.Sleep(100 * time.Millisecond)

	var dup, fresh int
	for i := range n {
		a := mustPost(t, ts.URL, "sms", sms(fmt.Sprint(i)))
		if a.status != 200 {
			t.Fatalf("retry %d: %d %+v", i, a.status, a.err)
		}
		if a.ok.Duplicate {
			dup++
		} else {
			fresh++
		}
	}
	snap := s.Snapshot()
	if len(snap.Deliveries) != n {
		t.Fatalf("%d deliveries, want %d", len(snap.Deliveries), n)
	}
	if dup == 0 || fresh == 0 || snap.Totals["sms"].HungAfterDelivery != dup {
		t.Fatalf("dup=%d fresh=%d totals=%+v: want both kinds of timeout", dup, fresh, snap.Totals["sms"])
	}
}

func TestRateLimit(t *testing.T) {
	s, ts := start(t, Config{RatePerChannel: 1})
	var limited answer
	for i := range rateBurst + 1 {
		a := mustPost(t, ts.URL, "sms", sms(fmt.Sprint(i)))
		if i < rateBurst && a.status != 200 {
			t.Fatalf("request %d within burst: %d", i, a.status)
		}
		limited = a
	}
	if limited.status != http.StatusTooManyRequests || limited.retryAfter != "1" {
		t.Fatalf("over the rate: %d Retry-After %q", limited.status, limited.retryAfter)
	}
	// Channels are limited independently.
	if a := mustPost(t, ts.URL, "email", SendRequest{MessageID: "e", To: "a@example.com", Subject: "s", Body: "b"}); a.status != 200 {
		t.Fatalf("email throttled by sms traffic: %d", a.status)
	}
	if s.Snapshot().Totals["sms"].Rejected[ErrRateLimited] != 1 {
		t.Fatal("rate_limited not counted")
	}
}

func TestThrottleWindow(t *testing.T) {
	s, ts := start(t, Config{ThrottleEvery: time.Nanosecond, ThrottleFor: 1500 * time.Millisecond})
	a := mustPost(t, ts.URL, "sms", sms("1"))
	if a.status != http.StatusTooManyRequests || a.retryAfter != "2" {
		t.Fatalf("inside window: %d Retry-After %q, want 429 with the rest of the window rounded up", a.status, a.retryAfter)
	}
	if s.Snapshot().Totals["sms"].Rejected["throttled"] != 1 {
		t.Fatal("throttled not counted")
	}
	// A throttled id was not kept: once the window is over it is delivered normally.
	s.mu.Lock()
	s.cfg.ThrottleEvery = 0
	s.mu.Unlock()
	if a := mustPost(t, ts.URL, "sms", sms("1")); a.status != 200 || a.ok.Duplicate {
		t.Fatalf("after window: %d %+v", a.status, a.ok)
	}
}

func TestHangEndsWithClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	hang(ctx)
	if time.Since(start) > time.Second {
		t.Fatal("hang outlived its client")
	}
}
