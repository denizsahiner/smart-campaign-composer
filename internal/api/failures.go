package api

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultFailureLimit = 50
	maxFailureLimit     = 500
)

// failureView is one recipient the provider rejected for good (messages.failure =
// 'permanent'): the data that needs fixing before they can be reached.
type failureView struct {
	MessageID int64     `json:"message_id"`
	UserID    int64     `json:"user_id"`
	FirstName string    `json:"first_name"`
	LastName  string    `json:"last_name"`
	Channel   string    `json:"channel"`
	Address   string    `json:"address"`
	Reason    string    `json:"reason"` // for people, in Turkish
	Error     string    `json:"error"`  // the provider's answer, as stored
	FailedAt  time.Time `json:"failed_at"`
}

const failureColumns = `m.id, m.user_id, u.first_name, u.last_name, m.channel, m.to_address, coalesce(m.last_error, ''), m.updated_at`

func scanFailure(row interface{ Scan(...any) error }) (failureView, error) {
	var f failureView
	err := row.Scan(&f.MessageID, &f.UserID, &f.FirstName, &f.LastName, &f.Channel, &f.Address, &f.Error, &f.FailedAt)
	f.Reason = failureReason(f.Channel, f.Error)
	return f, err
}

// failureReason turns the provider's answer ("400 invalid_recipient: domain does not
// exist") into a reason a marketer can act on.
func failureReason(channel, lastErr string) string {
	code, _, _ := strings.Cut(lastErr, ":")
	if _, after, ok := strings.Cut(code, " "); ok {
		code = after
	}
	switch {
	case code == "invalid_recipient" && channel == "sms":
		return "Geçersiz telefon numarası"
	case code == "invalid_recipient":
		return "Geçersiz e-posta adresi"
	case code == "invalid_request":
		return "Mesaj sağlayıcıya uygun değil"
	}
	return "Sağlayıcı reddetti"
}

// listFailures serves GET /api/campaigns/{id}/failures: the campaign's permanently failed
// messages, newest first, with their total. Paging is by message id: pass the response's
// next_before as before.
func (s *Server) listFailures(w http.ResponseWriter, r *http.Request) {
	c, ok := s.campaign(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	before := int64(0)
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "before must be a positive integer.")
			return
		}
		before = n
	}
	limit := defaultFailureLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxFailureLimit {
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("limit must be 1–%d.", maxFailureLimit))
			return
		}
		limit = n
	}

	var total int
	if err := s.pool.QueryRow(r.Context(), `
		SELECT count(*) FROM messages WHERE campaign_id = $1 AND failure = 'permanent'`, c.ID).Scan(&total); err != nil {
		internalError(w, r, err)
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT `+failureColumns+`
		FROM messages m JOIN users u ON u.id = m.user_id
		WHERE m.campaign_id = $1 AND m.failure = 'permanent' AND ($2 = 0 OR m.id < $2)
		ORDER BY m.id DESC LIMIT $3`, c.ID, before, limit+1)
	if err != nil {
		internalError(w, r, err)
		return
	}
	defer rows.Close()
	failures := []failureView{}
	for rows.Next() {
		f, err := scanFailure(rows)
		if err != nil {
			internalError(w, r, err)
			return
		}
		failures = append(failures, f)
	}
	if err := rows.Err(); err != nil {
		internalError(w, r, err)
		return
	}
	var next *int64
	if len(failures) > limit {
		failures = failures[:limit]
		next = &failures[limit-1].MessageID
	}
	writeJSON(w, http.StatusOK, map[string]any{"failures": failures, "total": total, "next_before": next})
}

// failuresCSV serves GET /api/campaigns/{id}/failures.csv: all of the campaign's
// permanently failed recipients as a file for whoever fixes the customer data. It is
// made for Turkish Excel: a UTF-8 BOM (or ş, ğ, ı come out garbled) and ';' as the
// separator (',' is the decimal separator there, so a comma file opens in one column).
func (s *Server) failuresCSV(w http.ResponseWriter, r *http.Request) {
	c, ok := s.campaign(w, r)
	if !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT `+failureColumns+`
		FROM messages m JOIN users u ON u.id = m.user_id
		WHERE m.campaign_id = $1 AND m.failure = 'permanent'
		ORDER BY m.id`, c.ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	defer rows.Close()

	var failures []failureView
	for rows.Next() {
		f, err := scanFailure(rows)
		if err != nil {
			internalError(w, r, err)
			return
		}
		failures = append(failures, f)
	}
	if err := rows.Err(); err != nil {
		internalError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="kampanya-%d-gonderilemeyenler.csv"`, c.ID))
	w.Write([]byte("\uFEFF"))
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	cw.UseCRLF = true
	cw.Write([]string{"Kullanıcı ID", "Ad", "Soyad", "Kanal", "Adres", "Neden", "Sağlayıcı yanıtı", "Zaman"})
	for _, f := range failures {
		cw.Write([]string{
			strconv.FormatInt(f.UserID, 10), cell(f.FirstName), cell(f.LastName), channelName(f.Channel),
			cell(f.Address), f.Reason, cell(f.Error), f.FailedAt.In(istanbul).Format("02.01.2006 15:04:05"),
		})
	}
	cw.Flush()
}

// cell keeps Excel from reading a value as a formula (CSV injection): a leading = + - @
// tab or CR gets a ' in front. Excel shows the rest as text, which also keeps a phone
// number such as +905321234567 from turning into 9,05E+11.
func cell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func channelName(ch string) string {
	if ch == "sms" {
		return "SMS"
	}
	return "E-posta"
}

// istanbul is where the people reading the file are. Without tzdata in the image, a fixed
// UTC+3, which is the same thing: Turkey has had no daylight saving time since 2016.
var istanbul = func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Istanbul"); err == nil {
		return loc
	}
	return time.FixedZone("UTC+3", 3*60*60)
}()
