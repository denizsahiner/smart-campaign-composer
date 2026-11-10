package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	sinkBuffer    = 2000            // records waiting to be written; more are dropped
	sinkBatch     = 200             // most rows per insert
	sinkInterval  = time.Second     // how often the buffer is written
	sinkTimeout   = 5 * time.Second // per insert
	maxValueRunes = 8000            // longer strings (a stack trace, say) are cut
)

// Sink writes warnings and errors to the app_logs table, for the logs page. Records are
// buffered and inserted in batches off the logging goroutine, so a slow or unreachable
// database never slows a request or a send: when the buffer is full, records are dropped
// and counted. Every record also goes to stdout (see New), so nothing is lost there.
type Sink struct {
	pool     *pgxpool.Pool
	fallback *slog.Logger // stdout only: the sink's own failures
	entries  chan entry
	dropped  atomic.Int64

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

type entry struct {
	ts         time.Time
	level      string
	service    string
	msg        string
	requestID  *string
	campaignID *int64
	attrs      map[string]any
}

// NewSink starts the writer. fallback is where the sink reports its own failures; it
// must not include the sink. Close flushes and stops it.
func NewSink(pool *pgxpool.Pool, fallback slog.Handler) *Sink {
	s := &Sink{
		pool: pool, fallback: slog.New(fallback), entries: make(chan entry, sinkBuffer),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go s.run()
	return s
}

// Handler returns the slog handler that feeds the sink: warn and above, or level if
// higher.
func (s *Sink) Handler(level slog.Level) slog.Handler {
	return &sinkHandler{sink: s, min: max(level, slog.LevelWarn)}
}

// Close writes what is buffered and stops the writer, giving up when ctx ends.
func (s *Sink) Close(ctx context.Context) {
	s.stopOnce.Do(func() { close(s.stop) })
	select {
	case <-s.done:
	case <-ctx.Done():
	}
}

func (s *Sink) enqueue(e entry) {
	select {
	case s.entries <- e:
	default:
		s.dropped.Add(1)
	}
}

func (s *Sink) run() {
	defer close(s.done)
	t := time.NewTicker(sinkInterval)
	defer t.Stop()
	var batch []entry
	for {
		select {
		case e := <-s.entries:
			if batch = append(batch, e); len(batch) >= sinkBatch {
				batch = s.flush(batch)
			}
		case <-t.C:
			batch = s.flush(batch)
		case <-s.stop:
		drain:
			for {
				select {
				case e := <-s.entries:
					batch = append(batch, e)
				default:
					break drain
				}
			}
			for len(batch) > 0 {
				n := min(len(batch), sinkBatch)
				s.flush(batch[:n])
				batch = batch[n:]
			}
			return
		}
	}
}

// flush inserts batch and returns it emptied for reuse. A failed insert is reported on
// stdout and the rows are given up: they are on stdout already.
func (s *Sink) flush(batch []entry) []entry {
	if n := s.dropped.Swap(0); n > 0 {
		s.fallback.Warn("log sink buffer full, records dropped", "dropped", n)
	}
	if len(batch) == 0 {
		return batch
	}
	ctx, cancel := context.WithTimeout(context.Background(), sinkTimeout)
	defer cancel()
	rows := make([][]any, len(batch))
	for i, e := range batch {
		attrs, err := json.Marshal(e.attrs)
		if err != nil {
			attrs = []byte(`{}`)
		}
		rows[i] = []any{e.ts, e.level, e.service, e.msg, e.requestID, e.campaignID, string(attrs)}
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"app_logs"},
		[]string{"ts", "level", "service", "msg", "request_id", "campaign_id", "attrs"},
		pgx.CopyFromRows(rows))
	if err != nil {
		s.fallback.Error("log sink write failed", "rows", len(batch), "err", err)
	}
	return batch[:0]
}

// Prune deletes rows older than retention and returns how many.
func Prune(ctx context.Context, pool *pgxpool.Pool, retention time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM app_logs WHERE ts < now() - $1 * interval '1 second'`,
		int64(retention/time.Second))
	return tag.RowsAffected(), err
}

// PruneEvery runs Prune now and then every interval until ctx ends.
func PruneEvery(ctx context.Context, pool *pgxpool.Pool, retention, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := Prune(ctx, pool, retention)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.WarnContext(ctx, "log prune failed", "err", err)
		case n > 0:
			slog.InfoContext(ctx, "old logs pruned", "rows", n, "retention", retention.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sinkHandler turns records into entries. service, request_id and campaign_id get their
// own columns; every other attribute goes into attrs, groups as nested objects.
type sinkHandler struct {
	sink   *Sink
	min    slog.Level
	attrs  []slog.Attr // from WithAttrs, already inside groups
	groups []string
}

func (h *sinkHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.min }

func (h *sinkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), nest(h.groups, attrs)...)
	return &c
}

func (h *sinkHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.groups = append(append([]string(nil), h.groups...), name)
	return &c
}

// nest wraps attrs in the open groups, innermost last.
func nest(groups []string, attrs []slog.Attr) []slog.Attr {
	for i := len(groups) - 1; i >= 0; i-- {
		attrs = []slog.Attr{{Key: groups[i], Value: slog.GroupValue(attrs...)}}
	}
	return attrs
}

func (h *sinkHandler) Handle(_ context.Context, r slog.Record) error {
	e := entry{ts: r.Time, level: levelName(r.Level), msg: truncate(r.Message), attrs: map[string]any{}}
	if e.ts.IsZero() {
		e.ts = time.Now()
	}
	var own []slog.Attr
	r.Attrs(func(a slog.Attr) bool { own = append(own, a); return true })
	for _, a := range append(append([]slog.Attr(nil), h.attrs...), nest(h.groups, own)...) {
		v := a.Value.Resolve()
		switch a.Key {
		case "service":
			e.service = v.String()
			continue
		case "request_id":
			id := v.String()
			e.requestID = &id
			continue
		case "campaign_id":
			if id, ok := asInt64(v); ok {
				e.campaignID = &id
				continue
			}
		}
		if a.Key != "" {
			put(e.attrs, a.Key, jsonValue(v))
		}
	}
	h.sink.enqueue(e)
	return nil
}

// put sets m[key], merging objects: a group opened with WithGroup gets attributes from
// both the logger and the call.
func put(m map[string]any, key string, v any) {
	old, ok1 := m[key].(map[string]any)
	add, ok2 := v.(map[string]any)
	if !ok1 || !ok2 {
		m[key] = v
		return
	}
	for k, x := range add {
		put(old, k, x)
	}
}

func levelName(l slog.Level) string {
	if l >= slog.LevelError {
		return "error"
	}
	return "warn"
}

func asInt64(v slog.Value) (int64, bool) {
	switch v.Kind() {
	case slog.KindInt64:
		return v.Int64(), true
	case slog.KindUint64:
		return int64(v.Uint64()), true
	}
	return 0, false
}

// jsonValue converts an attribute value to something encoding/json writes readably:
// errors and durations as their text, groups as objects.
func jsonValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return truncate(v.String())
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	case slog.KindGroup:
		m := map[string]any{}
		for _, a := range v.Group() {
			put(m, a.Key, jsonValue(a.Value.Resolve()))
		}
		return m
	}
	switch x := v.Any().(type) {
	case error:
		return truncate(x.Error())
	case fmt.Stringer:
		return truncate(x.String())
	case json.Marshaler:
		return x
	default:
		if _, err := json.Marshal(x); err == nil {
			return x
		}
		return truncate(fmt.Sprint(x))
	}
}

func truncate(s string) string {
	if utf8.RuneCountInString(s) <= maxValueRunes {
		return s
	}
	return strings.ToValidUTF8(string([]rune(s)[:maxValueRunes]), "") + "…"
}
