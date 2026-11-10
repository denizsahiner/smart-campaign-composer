package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/denizsahiner/smart-campaign-composer/internal/store"
)

// Monitoring configures GET /api/metrics and the delivery event stream.
type Monitoring struct {
	WorkerHealthURL string        // the worker's /healthz; empty = not reported
	ProviderURL     string        // the mock provider's base URL; empty = not reported
	PollInterval    time.Duration // how often an open event stream re-reads the counts
	PingInterval    time.Duration // keep-alive comment on an idle stream
}

func DefaultMonitoring() Monitoring {
	return Monitoring{PollInterval: time.Second, PingInterval: 15 * time.Second}
}

// WithMonitoring sets where the API finds the worker and the provider.
func (s *Server) WithMonitoring(m Monitoring) *Server {
	s.monitoring = m
	return s
}

// dependencyTimeout bounds each call to the worker or the provider: /api/metrics answers
// even when one of them is down.
const dependencyTimeout = time.Second

// rateWindow is how far back the send rate in a progress event looks.
const rateWindow = 5 * time.Second

var statuses = []string{"pending", "sending", "retry_scheduled", "sent", "failed", "skipped"}

// openStatuses are the ones still waiting for the worker.
var openStatuses = map[string]bool{"pending": true, "sending": true, "retry_scheduled": true}

// statusCounts is a count per message status, every status present (zero included) so
// the client never has to guess.
type statusCounts map[string]int

func newStatusCounts() statusCounts {
	c := statusCounts{}
	for _, st := range statuses {
		c[st] = 0
	}
	return c
}

func newChannelCounts() map[string]statusCounts {
	return map[string]statusCounts{"sms": newStatusCounts(), "email": newStatusCounts()}
}

type llmMetrics struct {
	Calls              int     `json:"calls"`
	ValidationFailures int     `json:"validation_failures"`
	Errors             int     `json:"errors"`
	AvgLatencyMS       float64 `json:"avg_latency_ms"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
}

type dependencyStatus struct {
	Status string          `json:"status"` // ok, unavailable (answered unhealthy), unreachable
	Error  string          `json:"error,omitempty"`
	Totals json.RawMessage `json:"totals,omitempty"` // provider only: its /deliveries totals
}

type metricsResponse struct {
	QueueDepth map[string]int          `json:"queue_depth"`
	Messages   map[string]statusCounts `json:"messages"`
	LLM        llmMetrics              `json:"llm"`
	Worker     *dependencyStatus       `json:"worker,omitempty"`
	Provider   *dependencyStatus       `json:"provider,omitempty"`
}

// metrics serves GET /api/metrics: queue and LLM numbers from the database, plus the
// worker's health and the provider's totals fetched live.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var (
		wg       sync.WaitGroup
		worker   *dependencyStatus
		provider *dependencyStatus
	)
	if u := s.monitoring.WorkerHealthURL; u != "" {
		wg.Go(func() { worker = fetchHealth(ctx, u) })
	}
	if u := s.monitoring.ProviderURL; u != "" {
		wg.Go(func() { provider = fetchProviderTotals(ctx, u) })
	}

	out := metricsResponse{QueueDepth: map[string]int{}, Messages: newChannelCounts()}
	err := s.messageCounts(ctx, out.Messages)
	if err == nil {
		err = s.pool.QueryRow(ctx, `
			SELECT count(*),
			       count(*) FILTER (WHERE validation_errors IS NOT NULL),
			       count(*) FILTER (WHERE error IS NOT NULL),
			       COALESCE(avg(latency_ms), 0),
			       COALESCE(sum(input_tokens), 0),
			       COALESCE(sum(output_tokens), 0)
			FROM llm_calls`).Scan(&out.LLM.Calls, &out.LLM.ValidationFailures, &out.LLM.Errors,
			&out.LLM.AvgLatencyMS, &out.LLM.InputTokens, &out.LLM.OutputTokens)
	}
	wg.Wait()
	if err != nil {
		internalError(w, r, err)
		return
	}
	for ch, counts := range out.Messages {
		for st, n := range counts {
			if openStatuses[st] {
				out.QueueDepth[ch] += n
			}
		}
	}
	out.Worker, out.Provider = worker, provider
	writeJSON(w, http.StatusOK, out)
}

// messageCounts fills counts per channel and status over all campaigns.
func (s *Server) messageCounts(ctx context.Context, into map[string]statusCounts) error {
	rows, err := s.pool.Query(ctx, `SELECT channel, status, count(*) FROM messages GROUP BY 1, 2`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ch, st string
		var n int
		if err := rows.Scan(&ch, &st, &n); err != nil {
			return err
		}
		if into[ch] != nil {
			into[ch][st] = n
		}
	}
	return rows.Err()
}

func getDependency(ctx context.Context, url string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, dependencyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, body, err
}

// fetchHealth reads a /healthz: ok on 200, unavailable with its reason otherwise.
func fetchHealth(ctx context.Context, url string) *dependencyStatus {
	code, body, err := getDependency(ctx, url)
	if err != nil {
		return &dependencyStatus{Status: "unreachable", Error: err.Error()}
	}
	if code != http.StatusOK {
		var h struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &h)
		if h.Error == "" {
			h.Error = fmt.Sprintf("healthz answered %d", code)
		}
		return &dependencyStatus{Status: "unavailable", Error: h.Error}
	}
	return &dependencyStatus{Status: "ok"}
}

// fetchProviderTotals reads the provider's delivery totals (requests, deliveries,
// duplicates answered, rejections by reason), so the dashboard can show why retries
// happened.
func fetchProviderTotals(ctx context.Context, baseURL string) *dependencyStatus {
	code, body, err := getDependency(ctx, strings.TrimRight(baseURL, "/")+"/deliveries?summary=1")
	if err != nil {
		return &dependencyStatus{Status: "unreachable", Error: err.Error()}
	}
	var r struct {
		Totals json.RawMessage `json:"totals"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &r) != nil {
		return &dependencyStatus{Status: "unavailable", Error: fmt.Sprintf("deliveries answered %d", code)}
	}
	return &dependencyStatus{Status: "ok", Totals: r.Totals}
}

// progress is the data of a progress (and the final done) event.
type progress struct {
	CampaignID int64                   `json:"campaign_id"`
	Status     string                  `json:"status"`
	Channels   map[string]statusCounts `json:"channels"`
	Total      map[string]int          `json:"total"`
	Rate       map[string]float64      `json:"rate_per_second"` // sends/s over the last 5 s
}

func (s *Server) readProgress(ctx context.Context, id int64) (progress, error) {
	p := progress{CampaignID: id, Channels: newChannelCounts(),
		Total: map[string]int{"sms": 0, "email": 0}, Rate: map[string]float64{"sms": 0, "email": 0}}
	if err := s.pool.QueryRow(ctx, `SELECT status FROM campaigns WHERE id = $1`, id).Scan(&p.Status); err != nil {
		return p, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT channel, status, count(*),
		       count(*) FILTER (WHERE sent_at > now() - $2 * interval '1 millisecond')
		FROM messages WHERE campaign_id = $1 GROUP BY 1, 2`, id, rateWindow.Milliseconds())
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var ch, st string
		var n, recent int
		if err := rows.Scan(&ch, &st, &n, &recent); err != nil {
			return p, err
		}
		if p.Channels[ch] == nil {
			continue
		}
		p.Channels[ch][st] = n
		p.Total[ch] += n
		if st == "sent" {
			p.Rate[ch] = float64(recent) / rateWindow.Seconds()
		}
	}
	return p, rows.Err()
}

// fingerprint identifies what a client would see changing; the rate is left out so an
// idle stream doesn't emit an event every poll.
func (p progress) fingerprint() string {
	b, _ := json.Marshal([]any{p.Status, p.Channels})
	return string(b)
}

// campaignEvents serves GET /api/campaigns/{id}/events: live delivery counts as SSE.
// It sends a progress event at once and whenever the counts change, and done (then
// closes) once the worker has marked the campaign completed, i.e. no message is left
// pending, sending or scheduled for retry.
func (s *Server) campaignEvents(w http.ResponseWriter, r *http.Request) {
	c, ok := s.campaign(w, r)
	if !ok {
		return
	}
	if c.Status != store.StatusApproved && c.Status != store.StatusCompleted {
		writeError(w, http.StatusConflict, "not_approved", "The campaign hasn't been approved, so nothing is being sent.")
		return
	}
	ctx := r.Context()
	sse := newSSE(w)
	poll := time.NewTicker(s.monitoring.PollInterval)
	defer poll.Stop()
	ping := time.NewTicker(s.monitoring.PingInterval)
	defer ping.Stop()

	last := ""
	for {
		p, err := s.readProgress(ctx, c.ID)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("progress read failed", "campaign_id", c.ID, "err", err)
				sse.event("error", map[string]string{"message": "Could not read delivery status; reconnect to retry."})
			}
			return
		}
		if fp := p.fingerprint(); fp != last {
			last = fp
			sse.event("progress", p)
		}
		if p.Status == store.StatusCompleted {
			sse.event("done", p)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			sse.comment("ping")
		case <-poll.C:
		}
	}
}
