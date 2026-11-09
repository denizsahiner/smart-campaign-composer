package worker

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryAfter string
		body       string
		want       Kind
	}{
		{"sent", 200, "", `{"provider_message_id":"sms_1","duplicate":false}`, Sent},
		{"duplicate is sent", 200, "", `{"provider_message_id":"sms_1","duplicate":true}`, Sent},
		{"200 unreadable", 200, "", `oops`, Retry},
		{"invalid recipient", 400, "", `{"error":"invalid_recipient"}`, Permanent},
		{"other 4xx", 422, "", `{}`, Permanent},
		{"in progress", 409, "", `{"error":"in_progress"}`, Retry},
		{"throttled", 429, "3", `{"error":"rate_limited"}`, Throttled},
		{"500", 500, "", `{"error":"provider_error"}`, Retry},
		{"503", 503, "", ``, Retry},
	}
	for _, c := range cases {
		if got := classify(c.status, c.retryAfter, []byte(c.body)); got.Kind != c.want {
			t.Errorf("%s: %v, want %v", c.name, got.Kind, c.want)
		}
	}
	if got := classify(429, "3", nil); got.RetryAfter != 3*time.Second {
		t.Errorf("Retry-After 3 → %v", got.RetryAfter)
	}
	for _, v := range []string{"", "0", "soon", "Wed, 21 Oct 2026 07:28:00 GMT"} {
		if got := parseRetryAfter(v); got != time.Second {
			t.Errorf("Retry-After %q → %v, want the 1s floor", v, got)
		}
	}
}

func TestSendRefusedConnectionIsUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens there now
	out := NewHTTPSender("http://"+addr, time.Second).Send(context.Background(), Message{ID: 1, Channel: "sms", To: "+905321234567", Body: "x"})
	if out.Kind != Unreachable {
		t.Fatalf("refused connection: %v (%s), want unreachable", out.Kind, out.Err)
	}
}

func TestSendTimeoutCounts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
		<-r.Context().Done()
	}))
	defer ts.Close()
	out := NewHTTPSender(ts.URL, 50*time.Millisecond).Send(context.Background(), Message{ID: 1, Channel: "sms", To: "+905321234567", Body: "x"})
	if out.Kind != Retry || out.Err != "timeout" {
		t.Fatalf("timeout: %v %q, want a counted retry", out.Kind, out.Err)
	}
}
