// Package worker sends queued messages to the provider: one loop per channel, each with
// its own claim query, goroutine pool and rate limiter, so a pause or a backlog on one
// channel never slows the other.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/denizsahiner/smart-campaign-composer/internal/backoff"
)

var Channels = []string{"sms", "email"}

const (
	// maxUnreachablePause caps the growing pause while the provider can't be reached.
	maxUnreachablePause = 30 * time.Second
	// staleAfter: a channel loop that hasn't iterated for this long makes /healthz fail.
	staleAfter = 10 * time.Second
	// writeTimeout bounds a result write; it runs even during shutdown.
	writeTimeout = 5 * time.Second
)

type Config struct {
	RatePerChannel float64 // sends/s per channel (the case's 100)
	Concurrency    int     // sends in flight per channel
	BatchSize      int     // most rows claimed at once
	Lease          time.Duration
	MaxAttempts    int
	BackoffBase    time.Duration
	PollInterval   time.Duration // wait when the queue is empty
}

func DefaultConfig() Config {
	return Config{
		RatePerChannel: 100,
		Concurrency:    32,
		BatchSize:      20,
		Lease:          30 * time.Second,
		MaxAttempts:    5,
		BackoffBase:    time.Second,
		PollInterval:   200 * time.Millisecond,
	}
}

type Worker struct {
	cfg      Config
	queue    *Queue
	sender   Sender
	channels []*channel
	jitter   func(time.Duration) time.Duration

	mu      sync.Mutex
	touched map[int64]bool // campaigns with a message that reached a final state
}

func New(cfg Config, queue *Queue, sender Sender) *Worker {
	w := &Worker{cfg: cfg, queue: queue, sender: sender, jitter: backoff.EqualJitter, touched: map[int64]bool{}}
	for _, name := range Channels {
		ch := &channel{name: name, limiter: rate.NewLimiter(rate.Limit(cfg.RatePerChannel), 1)}
		ch.lastLoop.Store(time.Now().UnixNano())
		w.channels = append(w.channels, ch)
	}
	return w
}

// channel is the per-channel state shared by its loop and its send goroutines.
type channel struct {
	name    string
	limiter *rate.Limiter // burst 1: never more than the rate in any one-second window

	mu                sync.Mutex
	pausedUntil       time.Time
	unreachableStreak int

	lastLoop atomic.Int64 // unix nanos of the last loop iteration
}

func (c *channel) pauseRemaining() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Until(c.pausedUntil)
}

// pause stops sends on the channel for d (never shortens a longer pause). It reports
// whether the channel wasn't already paused, so a pause is logged once, not once per
// in-flight send that hit it.
func (c *channel) pause(d time.Duration) (started bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	started = !c.pausedUntil.After(now)
	if until := now.Add(d); until.After(c.pausedUntil) {
		c.pausedUntil = until
	}
	return started
}

// unreachable starts the next, longer pause (1s, 2s, 4s ... 30s) and returns its length.
// Failures arriving during a pause (the other in-flight sends) join it instead of growing
// it; started is false for those.
func (c *channel) unreachable() (d time.Duration, started bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if left := time.Until(c.pausedUntil); left > 0 {
		return left, false
	}
	c.unreachableStreak++
	d = backoff.Exponential(time.Second, c.unreachableStreak, maxUnreachablePause)
	c.pausedUntil = time.Now().Add(d)
	return d, true
}

// reached resets the unreachable streak: the provider answered.
func (c *channel) reached() {
	c.mu.Lock()
	c.unreachableStreak = 0
	c.mu.Unlock()
}

// Run sends until ctx is cancelled, then stops claiming, lets in-flight sends finish,
// hands back claimed rows still waiting for the rate limiter, and returns. A row whose
// result couldn't be written stays 'sending' and is recovered when its lease expires.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, ch := range w.channels {
		wg.Go(func() { w.loop(ctx, ch) })
	}
	wg.Wait()
	w.flushCompleted(context.WithoutCancel(ctx))
}

func (w *Worker) loop(ctx context.Context, ch *channel) {
	slots := make(chan struct{}, w.cfg.Concurrency)
	freed := make(chan struct{}, 1)
	var inflight sync.WaitGroup
	defer inflight.Wait()

	for {
		ch.lastLoop.Store(time.Now().UnixNano())
		w.flushCompleted(ctx)
		if ctx.Err() != nil {
			return
		}
		if left := ch.pauseRemaining(); left > 0 {
			wait(ctx, nil, min(left, w.cfg.PollInterval))
			continue
		}
		free := cap(slots) - len(slots)
		if free == 0 {
			wait(ctx, freed, w.cfg.PollInterval)
			continue
		}
		msgs, err := w.queue.Claim(ctx, ch.name, min(free, w.cfg.BatchSize), w.cfg.Lease)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("claim failed", "channel", ch.name, "err", err)
			}
			wait(ctx, nil, w.cfg.PollInterval)
			continue
		}
		if len(msgs) == 0 {
			wait(ctx, nil, w.cfg.PollInterval)
			continue
		}
		for _, m := range msgs {
			slots <- struct{}{}
			inflight.Add(1)
			go func() {
				defer inflight.Done()
				w.process(ctx, ch, m)
				<-slots
				select {
				case freed <- struct{}{}:
				default:
				}
			}()
		}
	}
}

// process sends one claimed message and writes the outcome. A row is never held through
// a pause or a shutdown: it is handed back with its attempt refunded.
func (w *Worker) process(ctx context.Context, ch *channel, m Message) {
	if left := ch.pauseRemaining(); left > 0 {
		w.handBack(ctx, m, left)
		return
	}
	if err := ch.limiter.Wait(ctx); err != nil {
		w.handBack(ctx, m, 0) // shutting down
		return
	}
	if left := ch.pauseRemaining(); left > 0 {
		w.handBack(ctx, m, left)
		return
	}
	// The send itself isn't cut short by a shutdown; the HTTP timeout bounds it.
	out := w.sender.Send(context.WithoutCancel(ctx), m)
	w.apply(ctx, ch, m, out)
}

func (w *Worker) handBack(ctx context.Context, m Message, delay time.Duration) {
	w.write(ctx, m, "handed back", func(c context.Context) (bool, error) {
		return w.queue.Reschedule(c, m, delay, "", true)
	})
}

// apply writes the outcome of a send and logs it. Per-message lines are logged only once
// the write went through: a stale result belongs to a newer attempt, which logs its own.
// A sent message is debug (100 a second per channel would drown everything else), a
// scheduled retry warn, a message given up on error. A pause is logged once per channel.
func (w *Worker) apply(ctx context.Context, ch *channel, m Message, out Outcome) {
	if out.Kind != Unreachable {
		ch.reached()
	}
	log := slog.With("campaign_id", m.CampaignID, "message_id", m.ID, "channel", m.Channel, "attempt", m.Attempts)
	switch out.Kind {
	case Sent:
		if w.final(ctx, m, out, func(c context.Context) (bool, error) {
			return w.queue.MarkSent(c, m, out.ProviderMessageID)
		}) {
			log.Debug("message sent")
		}
	case Permanent:
		if w.final(ctx, m, out, func(c context.Context) (bool, error) {
			return w.queue.MarkFailed(c, m, out.Err, FailurePermanent)
		}) {
			log.Error("message failed", "reason", "rejected by provider", "err", out.Err)
		}
	case Retry:
		if m.Attempts >= w.cfg.MaxAttempts {
			if w.final(ctx, m, out, func(c context.Context) (bool, error) {
				return w.queue.MarkFailed(c, m, out.Err, FailureRetriesExhausted)
			}) {
				log.Error("message failed", "reason", "retries exhausted", "err", out.Err)
			}
			return
		}
		delay := w.jitter(backoff.Exponential(w.cfg.BackoffBase, m.Attempts, 0))
		if w.write(ctx, m, out.Kind.String(), func(c context.Context) (bool, error) {
			return w.queue.Reschedule(c, m, delay, out.Err, false)
		}) {
			log.Warn("send failed, retry scheduled", "retry_in", delay.Round(time.Millisecond), "err", out.Err)
		}
	case Throttled:
		if ch.pause(out.RetryAfter) {
			slog.Warn("channel throttled by provider", "channel", ch.name, "pause", out.RetryAfter)
		}
		log.Debug("message throttled, handed back")
		w.write(ctx, m, out.Kind.String(), func(c context.Context) (bool, error) {
			return w.queue.Reschedule(c, m, out.RetryAfter, out.Err, true)
		})
	case Unreachable:
		d, started := ch.unreachable()
		if started {
			slog.Warn("provider unreachable, channel paused", "channel", ch.name, "pause", d, "err", out.Err)
		}
		w.write(ctx, m, out.Kind.String(), func(c context.Context) (bool, error) {
			return w.queue.Reschedule(c, m, d, out.Err, true)
		})
	}
}

// final writes a terminal state and remembers the campaign for the completion check.
// It reports whether the write went through.
func (w *Worker) final(ctx context.Context, m Message, out Outcome, fn func(context.Context) (bool, error)) bool {
	if !w.write(ctx, m, out.Kind.String(), fn) {
		return false
	}
	w.mu.Lock()
	w.touched[m.CampaignID] = true
	w.mu.Unlock()
	return true
}

// write runs a fenced result write, even during shutdown. A write the fence rejected
// means the lease expired and the row was claimed again: the newer attempt owns it.
func (w *Worker) write(ctx context.Context, m Message, what string, fn func(context.Context) (bool, error)) bool {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	ok, err := fn(c)
	switch {
	case err != nil:
		// The row stays 'sending' and comes back when its lease expires.
		slog.Error("result write failed", "campaign_id", m.CampaignID, "message_id", m.ID, "outcome", what, "err", err)
	case !ok:
		slog.Warn("stale result dropped", "campaign_id", m.CampaignID, "message_id", m.ID, "attempt", m.Attempts, "outcome", what)
	}
	return ok && err == nil
}

func (w *Worker) flushCompleted(ctx context.Context) {
	w.mu.Lock()
	if len(w.touched) == 0 {
		w.mu.Unlock()
		return
	}
	ids := make([]int64, 0, len(w.touched))
	for id := range w.touched {
		ids = append(ids, id)
	}
	w.touched = map[int64]bool{}
	w.mu.Unlock()

	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := w.queue.CompleteCampaigns(c, ids); err != nil {
		slog.Error("completion check failed", "campaign_ids", ids, "err", err)
		w.mu.Lock()
		for _, id := range ids {
			w.touched[id] = true
		}
		w.mu.Unlock()
	}
}

// Healthy is nil when the database answers and every channel loop iterated recently, so
// a stuck loop is reported, not just a dead process.
func (w *Worker) Healthy(ctx context.Context) error {
	if err := w.queue.Ping(ctx); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	var stuck []string
	for _, ch := range w.channels {
		if time.Since(time.Unix(0, ch.lastLoop.Load())) > staleAfter {
			stuck = append(stuck, ch.name)
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("channel loop stalled: %v", stuck)
	}
	return nil
}

// HealthHandler serves GET /healthz: 200 {"status":"ok"} or 503 with the reason.
func (w *Worker) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		rw.Header().Set("Content-Type", "application/json")
		if err := w.Healthy(ctx); err != nil {
			rw.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(rw, `{"status":"unavailable","error":%q}`+"\n", err.Error())
			return
		}
		fmt.Fprintln(rw, `{"status":"ok"}`)
	})
	return mux
}

// wait sleeps d, or less if ctx ends or wake fires.
func wait(ctx context.Context, wake <-chan struct{}, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-wake:
	case <-t.C:
	}
}
