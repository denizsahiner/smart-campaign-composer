package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/denizsahiner/smart-campaign-composer/internal/provider"
)

// Kind is what the worker does with a provider answer (see the retry table in
// docs/ARCHITECTURE_DRAFT.md).
type Kind int

const (
	// Sent: 200, including a duplicate answer for an id the provider already sent.
	Sent Kind = iota
	// Permanent: a 4xx other than 409/429. The message can never succeed: failed, no retry.
	Permanent
	// Retry: 5xx, timeout, 409 in_progress, other transport errors. The request may have
	// reached the provider, so the attempt counts.
	Retry
	// Throttled: 429. Says nothing about the message: attempt refunded, channel paused.
	Throttled
	// Unreachable: the connection was never made, so the message certainly wasn't sent:
	// attempt refunded, channel paused with growing waits.
	Unreachable
)

func (k Kind) String() string {
	return [...]string{"sent", "permanent", "retry", "throttled", "unreachable"}[k]
}

type Outcome struct {
	Kind              Kind
	ProviderMessageID string        // Sent
	RetryAfter        time.Duration // Throttled
	Err               string        // everything but Sent; stored in last_error
}

// Sender posts one message to the provider.
type Sender interface {
	Send(ctx context.Context, m Message) Outcome
}

// HTTPSender talks to the mock provider's POST /sms and POST /email.
type HTTPSender struct {
	BaseURL string
	Client  *http.Client
}

func NewHTTPSender(baseURL string, timeout time.Duration) *HTTPSender {
	return &HTTPSender{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: timeout},
	}
}

func (s *HTTPSender) Send(ctx context.Context, m Message) Outcome {
	req := provider.SendRequest{MessageID: strconv.FormatInt(m.ID, 10), To: m.To, Body: m.Body}
	if m.Subject != nil {
		req.Subject = *m.Subject
	}
	body, _ := json.Marshal(req)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+"/"+m.Channel, bytes.NewReader(body))
	if err != nil {
		return Outcome{Kind: Permanent, Err: err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(hreq)
	if err != nil {
		return classifyTransport(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return classify(resp.StatusCode, resp.Header.Get("Retry-After"), raw)
}

// classify maps an HTTP answer to an outcome.
func classify(status int, retryAfter string, body []byte) Outcome {
	if status == http.StatusOK {
		var r provider.SendResponse
		if err := json.Unmarshal(body, &r); err != nil || r.ProviderMessageID == "" {
			// Sent, but unreadable: retrying is safe (the provider answers a repeat as a
			// duplicate with the id) and gets us the provider id.
			return Outcome{Kind: Retry, Err: "200 with unreadable body"}
		}
		return Outcome{Kind: Sent, ProviderMessageID: r.ProviderMessageID}
	}
	var e provider.ErrorResponse
	json.Unmarshal(body, &e)
	msg := fmt.Sprintf("%d %s", status, e.Error)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	switch {
	case status == http.StatusTooManyRequests:
		return Outcome{Kind: Throttled, RetryAfter: parseRetryAfter(retryAfter), Err: msg}
	case status == http.StatusConflict:
		// in_progress: the provider is still working on an earlier attempt of this id.
		return Outcome{Kind: Retry, Err: msg}
	case status >= 500:
		return Outcome{Kind: Retry, Err: msg}
	case status >= 400:
		return Outcome{Kind: Permanent, Err: msg}
	}
	return Outcome{Kind: Retry, Err: fmt.Sprintf("unexpected status %d", status)}
}

// parseRetryAfter reads delay-seconds (the provider never sends an HTTP date). Missing or
// unreadable means 1s, the shortest wait the header can express.
func parseRetryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return time.Second
	}
	return time.Duration(n) * time.Second
}

// classifyTransport tells "never connected" (refunded) from everything else (counted): a
// timeout or a dropped connection may have happened after the provider got the request.
func classifyTransport(err error) Outcome {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return Outcome{Kind: Unreachable, Err: "unreachable: " + err.Error()}
	case errors.As(err, &opErr) && opErr.Op == "dial" && !opErr.Timeout():
		return Outcome{Kind: Unreachable, Err: "unreachable: " + err.Error()}
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return Outcome{Kind: Retry, Err: "timeout"}
	}
	return Outcome{Kind: Retry, Err: err.Error()}
}
