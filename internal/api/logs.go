package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultLogLimit = 100
	maxLogLimit     = 500
)

type logView struct {
	ID         int64           `json:"id"`
	TS         time.Time       `json:"ts"`
	Level      string          `json:"level"`
	Service    string          `json:"service"`
	Message    string          `json:"message"`
	RequestID  *string         `json:"request_id"`
	CampaignID *int64          `json:"campaign_id"`
	Attrs      json.RawMessage `json:"attrs"`
}

// logFilter is the WHERE clause built from the query string, with its arguments.
type logFilter struct {
	where []string
	args  []any
}

func (f *logFilter) add(cond string, arg any) {
	f.args = append(f.args, arg)
	f.where = append(f.where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(f.args))))
}

func (f *logFilter) sql() string {
	if len(f.where) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(f.where, " AND ")
}

// parseLogFilter reads level (the minimum: warn or error), service, campaign_id,
// request_id and q (text in the message or the attributes). It writes a 400 and returns
// false on a bad value.
func parseLogFilter(w http.ResponseWriter, r *http.Request) (*logFilter, bool) {
	q := r.URL.Query()
	f := &logFilter{}
	switch q.Get("level") {
	case "", "warn":
	case "error":
		f.add("level = ?", "error")
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "level must be warn or error.")
		return nil, false
	}
	if v := q.Get("service"); v != "" {
		f.add("service = ?", v)
	}
	if v := q.Get("campaign_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "campaign_id must be a positive integer.")
			return nil, false
		}
		f.add("campaign_id = ?", id)
	}
	if v := q.Get("request_id"); v != "" {
		f.add("request_id = ?", v)
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(v) + "%"
		f.add("(msg ILIKE ? OR attrs::text ILIKE ?)", pattern) // both ? become the same $n
	}
	return f, true
}

// listLogs serves GET /api/logs: stored warnings and errors, newest first, filtered as
// in parseLogFilter. Paging is by id: pass the response's next_before as before.
func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	f, ok := parseLogFilter(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if v := q.Get("before"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "before must be an integer.")
			return
		}
		f.add("id < ?", id)
	}
	limit := defaultLogLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLogLimit {
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("limit must be 1–%d.", maxLogLimit))
			return
		}
		limit = n
	}

	rows, err := s.pool.Query(r.Context(), `
		SELECT id, ts, level, service, msg, request_id, campaign_id, attrs
		FROM app_logs `+f.sql()+`
		ORDER BY id DESC LIMIT `+strconv.Itoa(limit+1), f.args...)
	if err != nil {
		internalError(w, r, err)
		return
	}
	defer rows.Close()
	logs := []logView{}
	for rows.Next() {
		var l logView
		if err := rows.Scan(&l.ID, &l.TS, &l.Level, &l.Service, &l.Message, &l.RequestID, &l.CampaignID, &l.Attrs); err != nil {
			internalError(w, r, err)
			return
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		internalError(w, r, err)
		return
	}
	var next *int64
	if len(logs) > limit {
		logs = logs[:limit]
		next = &logs[limit-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs, "next_before": next})
}

// logSummary serves GET /api/logs/summary: how many errors and warnings match the
// filter, optionally only those of the last since_hours hours. The campaign page and
// the monitoring page use it to say whether there is anything to look at.
func (s *Server) logSummary(w http.ResponseWriter, r *http.Request) {
	f, ok := parseLogFilter(w, r)
	if !ok {
		return
	}
	if v := r.URL.Query().Get("since_hours"); v != "" {
		h, err := strconv.Atoi(v)
		if err != nil || h < 1 {
			writeError(w, http.StatusBadRequest, "bad_request", "since_hours must be a positive integer.")
			return
		}
		f.add("ts > now() - ? * interval '1 hour'", h)
	}
	var out struct {
		Errors   int `json:"errors"`
		Warnings int `json:"warnings"`
	}
	err := s.pool.QueryRow(r.Context(), `
		SELECT count(*) FILTER (WHERE level = 'error'), count(*) FILTER (WHERE level = 'warn')
		FROM app_logs `+f.sql(), f.args...).Scan(&out.Errors, &out.Warnings)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
