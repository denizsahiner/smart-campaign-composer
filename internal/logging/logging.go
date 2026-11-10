// Package logging sets up the structured logger shared by every service: JSON (or text)
// on stdout, a service attribute on every record, request-scoped attributes carried in
// the context, and an optional database sink for warnings and errors (see Sink).
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
)

type Config struct {
	Service string
	Level   slog.Level
	Format  string // "json" or "text"
	Out     io.Writer
}

// ConfigFromEnv reads LOG_LEVEL (debug, info, warn, error; default info) and LOG_FORMAT
// (json, text; default json). A bad value is an error, not a silent default.
func ConfigFromEnv(service string) (Config, error) {
	cfg := Config{Service: service, Level: slog.LevelInfo, Format: "json", Out: os.Stdout}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := cfg.Level.UnmarshalText([]byte(v)); err != nil {
			return cfg, fmt.Errorf("invalid environment: LOG_LEVEL=%q: want debug, info, warn or error", v)
		}
	}
	if v := strings.ToLower(os.Getenv("LOG_FORMAT")); v != "" {
		if v != "json" && v != "text" {
			return cfg, fmt.Errorf("invalid environment: LOG_FORMAT=%q: want json or text", v)
		}
		cfg.Format = v
	}
	return cfg, nil
}

// Console is the stdout handler alone. The sink reports its own failures here, so they
// never loop back into the sink.
func (c Config) Console() slog.Handler {
	// Durations as text ("1.5s"): the JSON handler would otherwise write nanoseconds.
	opts := &slog.HandlerOptions{Level: c.Level, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Value.Kind() == slog.KindDuration {
			a.Value = slog.StringValue(a.Value.Duration().String())
		}
		return a
	}}
	var h slog.Handler
	if c.Format == "text" {
		h = slog.NewTextHandler(c.Out, opts)
	} else {
		h = slog.NewJSONHandler(c.Out, opts)
	}
	return h.WithAttrs([]slog.Attr{slog.String("service", c.Service)})
}

// New returns the logger for cfg: stdout plus any extra handlers (the database sink),
// with the attributes of the context's scope added to every record.
func New(cfg Config, extra ...slog.Handler) *slog.Logger {
	h := cfg.Console()
	if len(extra) > 0 {
		hs := []slog.Handler{h}
		for _, e := range extra {
			hs = append(hs, e.WithAttrs([]slog.Attr{slog.String("service", cfg.Service)}))
		}
		h = slog.NewMultiHandler(hs...)
	}
	return slog.New(scopeHandler{next: h})
}

// scope holds attributes that every record logged with the context gets, such as a
// request's request_id and, once the handler knows it, its campaign_id. It is mutable so
// code deep in a request can add to it without threading a new context back up.
type scope struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

type scopeKey struct{}

// WithScope returns a context with a new, empty attribute scope.
func WithScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, &scope{})
}

// Add sets attributes (key-value pairs, as for slog) on the context's scope; a key that
// is already there is replaced. Without a scope it does nothing.
func Add(ctx context.Context, args ...any) {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	if s == nil {
		return
	}
	r := slog.Record{}
	r.Add(args...)
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool {
		for i := range s.attrs {
			if s.attrs[i].Key == a.Key {
				s.attrs[i] = a
				return true
			}
		}
		s.attrs = append(s.attrs, a)
		return true
	})
}

func scopeAttrs(ctx context.Context) []slog.Attr {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]slog.Attr(nil), s.attrs...)
}

// RequestID is the request_id in the context's scope, "" if none.
func RequestID(ctx context.Context) string {
	for _, a := range scopeAttrs(ctx) {
		if a.Key == "request_id" {
			return a.Value.String()
		}
	}
	return ""
}

// scopeHandler adds the scope's attributes to each record, except keys the logger or the
// record sets itself (a log call naming campaign_id explicitly wins over the scope's).
type scopeHandler struct {
	next  slog.Handler
	keys  map[string]bool // top-level keys from WithAttrs
	group bool            // inside WithGroup: scope attributes are not added (they'd land in the group)
}

func (h scopeHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h scopeHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := scopeAttrs(ctx); len(attrs) > 0 && !h.group {
		own := map[string]bool{}
		r.Attrs(func(a slog.Attr) bool { own[a.Key] = true; return true })
		r = r.Clone()
		for _, a := range attrs {
			if !own[a.Key] && !h.keys[a.Key] {
				r.AddAttrs(a)
			}
		}
	}
	return h.next.Handle(ctx, r)
}

func (h scopeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	keys := map[string]bool{}
	for k := range h.keys {
		keys[k] = true
	}
	if !h.group {
		for _, a := range attrs {
			keys[a.Key] = true
		}
	}
	return scopeHandler{next: h.next.WithAttrs(attrs), keys: keys, group: h.group}
}

func (h scopeHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return scopeHandler{next: h.next.WithGroup(name), keys: h.keys, group: true}
}

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// NewRequestID returns incoming if it looks like a request ID (so a caller's or a
// proxy's ID carries through), otherwise a fresh random one.
func NewRequestID(incoming string) string {
	if validRequestID.MatchString(incoming) {
		return incoming
	}
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
