// Package api is the HTTP API for the campaign flow: brief → audience → messages (SSE).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denizsahiner/smart-campaign-composer/internal/compose"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
	"github.com/denizsahiner/smart-campaign-composer/internal/store"
	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

type Server struct {
	pool      *pgxpool.Pool
	store     *store.Store
	composer  *compose.Composer
	validator *segment.Validator
	now       func() time.Time

	monitoring Monitoring
	staticDir  string // the built frontend; empty = API only

	// busy holds campaigns with an LLM step in progress, so a double click or a second
	// tab can't run two generations (or a generation and an audience change) at once.
	// In memory: the API runs as a single replica.
	mu   sync.Mutex
	busy map[int64]bool
}

func NewServer(pool *pgxpool.Pool, composer *compose.Composer, validator *segment.Validator) *Server {
	return &Server{
		pool: pool, store: store.New(pool), composer: composer, validator: validator,
		now: time.Now, busy: map[int64]bool{}, monitoring: DefaultMonitoring(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/campaigns", s.listCampaigns)
	mux.HandleFunc("POST /api/campaigns", s.createCampaign)
	mux.HandleFunc("GET /api/campaigns/{id}", s.getCampaign)
	mux.HandleFunc("POST /api/campaigns/{id}/audience", s.rerunAudience)
	mux.HandleFunc("POST /api/campaigns/{id}/acknowledge", s.acknowledge)
	mux.HandleFunc("POST /api/campaigns/{id}/messages", s.generateMessages)
	mux.HandleFunc("POST /api/campaigns/{id}/audience/refresh", s.refreshAudience)
	mux.HandleFunc("POST /api/campaigns/{id}/approve", s.approve)
	mux.HandleFunc("GET /api/campaigns/{id}/events", s.campaignEvents)
	mux.HandleFunc("GET /api/metrics", s.metrics)
	if s.staticDir != "" {
		// The least specific GET pattern: wrong methods on API routes still get 405.
		mux.Handle("GET /", staticHandler(s.staticDir))
	}
	return logRequests(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "db": "down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "db": "ok"})
}

// lock marks a campaign busy; false if it already is.
func (s *Server) lock(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		return false
	}
	s.busy[id] = true
	return true
}

func (s *Server) unlock(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, id)
}

// apiError is the body of every error response.
type apiError struct {
	Code    string                  `json:"code"`
	Message string                  `json:"message"`
	Errors  []validation.FieldError `json:"errors,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string, errs ...validation.FieldError) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg, Errors: errs}})
}

func internalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "Something went wrong on our side.")
}

// decode reads a small JSON body strictly.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// campaign loads the campaign named in the path, writing 404 if there is none.
func (s *Server) campaign(w http.ResponseWriter, r *http.Request) (store.Campaign, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "Campaign not found.")
		return store.Campaign{}, false
	}
	c, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Campaign not found.")
		return store.Campaign{}, false
	}
	if err != nil {
		internalError(w, r, err)
		return store.Campaign{}, false
	}
	return c, true
}

func editable(c store.Campaign) bool {
	return c.Status == store.StatusDraft || c.Status == store.StatusReady
}

func writeNotEditable(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "not_editable", "The campaign has been approved and can no longer be changed.")
}

func writeBusy(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "busy", "Claude is already working on this campaign.")
}

// statusRecorder captures the status code for logging, and whether the response has
// started (after that a panic can no longer turn it into a 500). Unwrap lets
// http.ResponseController reach the underlying writer's Flush for SSE.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	started bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.started = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.started = true
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests logs every request (except health checks) and recovers handler panics.
// The recovery runs first, so a recovered panic is logged as a 500.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if r.URL.Path != "/healthz" {
				slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
					"duration", time.Since(start).Round(time.Millisecond))
			}
		}()
		defer recoverPanic(rec, r)
		next.ServeHTTP(rec, r)
	})
}

// recoverPanic turns a handler panic into a logged 500 with the stack trace, instead of
// net/http's plain-text log line and a dropped connection. If the response has already
// started (an SSE stream, say) the status can't change, so the connection is aborted
// rather than ended as if the response were complete. http.ErrAbortHandler is that
// deliberate abort and is passed through untouched.
func recoverPanic(w *statusRecorder, r *http.Request) {
	v := recover()
	if v == nil {
		return
	}
	if v == http.ErrAbortHandler {
		panic(v)
	}
	slog.Error("panic", "method", r.Method, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
	if w.started {
		panic(http.ErrAbortHandler)
	}
	writeError(w, http.StatusInternalServerError, "internal", "Something went wrong on our side.")
}
