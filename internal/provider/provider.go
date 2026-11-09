// Package provider is the mock SMS/email provider: a synchronous HTTP service that
// accepts sends, deduplicates them by message_id and injects the failures a real
// provider produces (5xx, throttling with Retry-After, timeouts, latency), so the
// worker's retry and rate-limit logic runs on every send.
//
// Nothing is delivered anywhere; a "delivery" is a record kept in memory and listed by
// GET /deliveries, which is the evidence that retries never became duplicate sends.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	ChannelSMS   = "sms"
	ChannelEmail = "email"

	// maxHang bounds an injected timeout in case the client never gives up.
	maxHang = 30 * time.Second
	// rateBurst is the tolerance of the provider's token bucket: a client keeping the
	// configured rate is never throttled by it, even when requests arrive slightly bunched.
	rateBurst = 10
)

// Error codes returned in the "error" field.
const (
	ErrInvalidRecipient = "invalid_recipient"
	ErrInvalidRequest   = "invalid_request"
	ErrInProgress       = "in_progress"
	ErrRateLimited      = "rate_limited"
	ErrProvider         = "provider_error"
)

type Config struct {
	RatePerChannel float64       // requests/s per channel; 0 disables the limit
	ErrorRate      float64       // share of requests answered with a 5xx
	TimeoutRate    float64       // share of requests that hang until the client gives up
	ThrottleEvery  time.Duration // mean gap between throttle windows per channel; 0 disables
	ThrottleFor    time.Duration // length of a throttle window
	LatencyMin     time.Duration
	LatencyMax     time.Duration
	Seed           uint64 // 0 means random
}

// DefaultConfig are the compose defaults: every run exercises retries and throttling.
func DefaultConfig() Config {
	return Config{
		RatePerChannel: 100,
		ErrorRate:      0.05,
		TimeoutRate:    0.01,
		ThrottleEvery:  30 * time.Second,
		ThrottleFor:    2 * time.Second,
		LatencyMin:     50 * time.Millisecond,
		LatencyMax:     300 * time.Millisecond,
	}
}

type SendRequest struct {
	MessageID string `json:"message_id"`
	To        string `json:"to"`
	Subject   string `json:"subject,omitempty"`
	Body      string `json:"body"`
}

type SendResponse struct {
	ProviderMessageID string `json:"provider_message_id"`
	Duplicate         bool   `json:"duplicate"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// Delivery is one message that was actually "sent". Requests counts every request that
// carried its message_id, retries and duplicates included.
type Delivery struct {
	MessageID         string    `json:"message_id"`
	Channel           string    `json:"channel"`
	To                string    `json:"to"`
	ProviderMessageID string    `json:"provider_message_id"`
	DeliveredAt       time.Time `json:"delivered_at"`
	Requests          int       `json:"requests"`
}

// ChannelTotals counts what the provider saw on one channel. Rejected counts requests
// that delivered nothing, keyed by reason: invalid_recipient, invalid_request,
// in_progress, rate_limited (over the rate), throttled (inside a throttle window),
// server_error, timeout. HungAfterDelivery counts deliveries whose answer was never sent:
// their retries must come back as duplicates.
type ChannelTotals struct {
	Requests          int            `json:"requests"`
	Deliveries        int            `json:"deliveries"`
	Duplicates        int            `json:"duplicates"`
	HungAfterDelivery int            `json:"hung_after_delivery"`
	Rejected          map[string]int `json:"rejected"`
}

type DeliveriesResponse struct {
	Totals     map[string]*ChannelTotals `json:"totals"`
	Deliveries []Delivery                `json:"deliveries"`
}

// entry is the idempotency record of one message_id: in progress until the delivery is
// recorded, then sent. Failures delete it, so they are never cached.
type entry struct {
	inProgress bool
	delivery   *Delivery
}

type channelState struct {
	limiter        *rate.Limiter // nil when unlimited
	throttleUntil  time.Time
	nextThrottleAt time.Time
}

type Server struct {
	cfg Config
	now func() time.Time

	mu       sync.Mutex
	rng      *rand.Rand
	ids      map[string]*entry
	requests map[string]int // per message_id
	order    []string       // delivered ids in delivery order
	totals   map[string]*ChannelTotals
	channels map[string]*channelState
}

func New(cfg Config) *Server {
	seed := cfg.Seed
	if seed == 0 {
		seed = rand.Uint64()
	}
	s := &Server{
		cfg:      cfg,
		now:      time.Now,
		rng:      rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		ids:      map[string]*entry{},
		requests: map[string]int{},
		totals:   map[string]*ChannelTotals{},
		channels: map[string]*channelState{},
	}
	now := s.now()
	for _, ch := range []string{ChannelSMS, ChannelEmail} {
		st := &channelState{}
		if cfg.RatePerChannel > 0 {
			st.limiter = rate.NewLimiter(rate.Limit(cfg.RatePerChannel), rateBurst)
		}
		if cfg.ThrottleEvery > 0 {
			st.nextThrottleAt = now.Add(s.throttleGap())
		}
		s.channels[ch] = st
		s.totals[ch] = &ChannelTotals{Rejected: map[string]int{}}
	}
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sms", func(w http.ResponseWriter, r *http.Request) { s.send(w, r, ChannelSMS) })
	mux.HandleFunc("POST /email", func(w http.ResponseWriter, r *http.Request) { s.send(w, r, ChannelEmail) })
	mux.HandleFunc("GET /deliveries", s.deliveries)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

var turkishMobile = regexp.MustCompile(`^\+905\d{9}$`)

// validate checks the request shape, then the recipient. A recipient error is permanent:
// retrying the same message can never succeed.
func validate(channel string, req SendRequest) (code, msg string) {
	switch {
	case strings.TrimSpace(req.MessageID) == "":
		return ErrInvalidRequest, "message_id is required"
	case strings.TrimSpace(req.To) == "":
		return ErrInvalidRequest, "to is required"
	case strings.TrimSpace(req.Body) == "":
		return ErrInvalidRequest, "body is required"
	case channel == ChannelEmail && strings.TrimSpace(req.Subject) == "":
		return ErrInvalidRequest, "subject is required"
	case channel == ChannelSMS && req.Subject != "":
		return ErrInvalidRequest, "sms has no subject"
	}
	if channel == ChannelSMS {
		if !turkishMobile.MatchString(req.To) {
			return ErrInvalidRecipient, "not a Turkish mobile number"
		}
		return "", ""
	}
	at := strings.LastIndexByte(req.To, '@')
	if at <= 0 || at == len(req.To)-1 {
		return ErrInvalidRecipient, "malformed email address"
	}
	if domain := strings.ToLower(req.To[at+1:]); domain == "invalid" || strings.HasSuffix(domain, ".invalid") {
		return ErrInvalidRecipient, "domain does not exist"
	}
	return "", ""
}

// fault is what the dice decided for one accepted request.
type fault int

const (
	faultNone fault = iota
	faultServerError
	faultHangBefore // hang without processing: nothing sent
	faultHangAfter  // record the delivery, then hang: sent, but the client never hears
)

func (s *Server) send(w http.ResponseWriter, r *http.Request, channel string) {
	var req SendRequest
	body := http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	// Read the body to EOF: only then does net/http watch the connection and cancel
	// r.Context() when the client disconnects, which is what ends an injected hang.
	io.Copy(io.Discard, body)
	if err != nil {
		s.mu.Lock()
		s.totals[channel].Requests++
		s.totals[channel].Rejected[ErrInvalidRequest]++
		s.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, ErrorResponse{ErrInvalidRequest, "body must be a JSON send request"})
		return
	}

	// Validation → idempotency → throttle window and rate limit → injected failure, all
	// decided under one lock; latency and hangs happen outside it.
	s.mu.Lock()
	tot := s.totals[channel]
	tot.Requests++
	if req.MessageID != "" {
		s.requests[req.MessageID]++
	}
	if code, msg := validate(channel, req); code != "" {
		tot.Rejected[code]++
		s.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, ErrorResponse{code, msg})
		return
	}
	if e, ok := s.ids[req.MessageID]; ok {
		if e.inProgress {
			tot.Rejected[ErrInProgress]++
			s.mu.Unlock()
			writeJSON(w, http.StatusConflict, ErrorResponse{Error: ErrInProgress})
			return
		}
		// Already sent: the original answer, no second delivery and no rate-limit token.
		tot.Duplicates++
		id := e.delivery.ProviderMessageID
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, SendResponse{ProviderMessageID: id, Duplicate: true})
		return
	}
	if wait, reason := s.admit(channel); wait > 0 {
		tot.Rejected[reason]++
		s.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, ErrorResponse{Error: ErrRateLimited})
		return
	}
	f := s.roll()
	latency := s.latency()
	s.ids[req.MessageID] = &entry{inProgress: true}
	s.mu.Unlock()

	switch f {
	case faultServerError:
		s.fail(channel, req.MessageID, "server_error")
		code := http.StatusInternalServerError
		if s.coin() {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, ErrorResponse{Error: ErrProvider})
		return
	case faultHangBefore:
		hang(r.Context())
		s.fail(channel, req.MessageID, "timeout")
		return
	}

	if !sleep(r.Context(), latency) {
		// The client gave up during the latency: not processed, so not sent.
		s.fail(channel, req.MessageID, "timeout")
		return
	}
	d := s.deliver(channel, req)
	if f == faultHangAfter {
		s.mu.Lock()
		tot.HungAfterDelivery++
		s.mu.Unlock()
		hang(r.Context())
		return
	}
	writeJSON(w, http.StatusOK, SendResponse{ProviderMessageID: d.ProviderMessageID})
}

// admit applies the throttle window, then the rate limit. It returns how long the client
// should wait (0 = admitted) and the rejection reason. Called with s.mu held.
func (s *Server) admit(channel string) (time.Duration, string) {
	st := s.channels[channel]
	now := s.now()
	if s.cfg.ThrottleEvery > 0 {
		if !now.Before(st.nextThrottleAt) && !now.Before(st.throttleUntil) {
			st.throttleUntil = now.Add(s.cfg.ThrottleFor)
			st.nextThrottleAt = st.throttleUntil.Add(s.throttleGap())
		}
		if now.Before(st.throttleUntil) {
			return max(st.throttleUntil.Sub(now), time.Second), "throttled"
		}
	}
	if st.limiter != nil && !st.limiter.AllowN(now, 1) {
		return time.Second, ErrRateLimited
	}
	return 0, ""
}

// throttleGap draws the time to the next throttle window: exponential with mean
// ThrottleEvery, so windows come at random, like load on a real provider.
func (s *Server) throttleGap() time.Duration {
	return time.Duration(s.rng.ExpFloat64() * float64(s.cfg.ThrottleEvery))
}

// roll decides the injected failure for an admitted request. Called with s.mu held.
func (s *Server) roll() fault {
	x := s.rng.Float64()
	switch {
	case x < s.cfg.ErrorRate:
		return faultServerError
	case x < s.cfg.ErrorRate+s.cfg.TimeoutRate:
		if s.rng.IntN(2) == 0 {
			return faultHangBefore
		}
		return faultHangAfter
	}
	return faultNone
}

func (s *Server) latency() time.Duration {
	span := s.cfg.LatencyMax - s.cfg.LatencyMin
	if span <= 0 {
		return s.cfg.LatencyMin
	}
	return s.cfg.LatencyMin + time.Duration(s.rng.Int64N(int64(span)+1))
}

func (s *Server) coin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.IntN(2) == 0
}

// fail forgets an in-progress id, so the next attempt is processed normally.
func (s *Server) fail(channel, id, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ids, id)
	s.totals[channel].Rejected[reason]++
}

func (s *Server) deliver(channel string, req SendRequest) Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := &Delivery{
		MessageID:         req.MessageID,
		Channel:           channel,
		To:                req.To,
		ProviderMessageID: fmt.Sprintf("%s_%016x", channel, s.rng.Uint64()),
		DeliveredAt:       s.now(),
	}
	s.ids[req.MessageID] = &entry{delivery: d}
	s.order = append(s.order, req.MessageID)
	s.totals[channel].Deliveries++
	return *d
}

func (s *Server) deliveries(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Snapshot())
}

// Snapshot returns the totals and every delivery, in delivery order.
func (s *Server) Snapshot() DeliveriesResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := DeliveriesResponse{Totals: map[string]*ChannelTotals{}, Deliveries: make([]Delivery, 0, len(s.order))}
	for ch, t := range s.totals {
		c := *t
		c.Rejected = map[string]int{}
		for k, v := range t.Rejected {
			c.Rejected[k] = v
		}
		out.Totals[ch] = &c
	}
	for _, id := range s.order {
		d := *s.ids[id].delivery
		d.Requests = s.requests[id]
		out.Deliveries = append(out.Deliveries, d)
	}
	return out
}

// hang blocks until the client disconnects (its timeout), at most maxHang.
func hang(ctx context.Context) { sleep(ctx, maxHang) }

// sleep waits d; false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
