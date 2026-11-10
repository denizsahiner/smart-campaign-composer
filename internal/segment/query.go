package segment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Per-channel reachability. This is the mandatory consent filter: it's applied in code on
// top of every segment, never comes from the DSL, and is shared by the reach count and
// the enqueue query so the preview and the actual send always agree.
const (
	SMSReachable   = "u.sms_consent AND u.phone IS NOT NULL"
	EmailReachable = "u.email_consent AND u.email IS NOT NULL"
)

// lastCartAt is the user's most recent cart_add, NULL if there is none. Served by the
// (user_id, type, created_at) index as a backward index scan with limit 1.
const lastCartAt = "(SELECT max(e.created_at) FROM user_events e WHERE e.user_id = u.id AND e.type = 'cart_add')"

var ErrNotValidated = errors.New("segment: Build called with a segment that was not produced by Validator.Parse")

// Build translates a validated segment into a boolean SQL expression over `users u`.
// Its values are appended to args and referenced as $len(args)+1, …, so the caller can
// combine it with its own parameters. Column names and SQL text come only from constants
// in this file; nothing from the segment is interpolated into the SQL string.
//
// Day semantics: cart_added_days_ago is DaysAgo(asOf, last cart_add), i.e. calendar days
// in Location. With asOf = Nov 7 14:00, "lte 30" starts at Oct 8 00:00 Istanbul time, so
// Oct 8 00:00:00 matches and Oct 7 23:59:59 doesn't. The boundary instants are computed
// in Go and passed as timestamptz parameters.
//
// asOf is the reference time, not now(): the caller pins it when the audience is
// previewed and reuses it at approval, so the day window can't shift in between.
//
// Users without a cart_add never match cart_added_days_ago or purchased_after_cart.
func Build(s Segment, asOf time.Time, args []any) (string, []any, error) {
	if !s.validated {
		return "", nil, ErrNotValidated
	}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	conds := make([]string, 0, len(s.Rules))
	for _, r := range s.Rules {
		var c string
		switch {
		case r.Field == FieldCity && r.Op == OpEq:
			c = "u.city = " + arg(r.Value.(string))
		case r.Field == FieldCity && r.Op == OpIn:
			c = "u.city = ANY(" + arg(r.Value.([]string)) + ")"
		case r.Field == FieldLanguage && r.Op == OpEq:
			c = "u.language = " + arg(r.Value.(string))
		case r.Field == FieldCartAddedDaysAgo && r.Op == OpLte:
			// days ago <= n  ⇔  on or after 00:00 of the day n days back
			c = lastCartAt + " >= " + arg(dayStart(asOf, r.Value.(int)))
		case r.Field == FieldCartAddedDaysAgo && r.Op == OpGte:
			// days ago >= n  ⇔  before 00:00 of the day n-1 days back
			c = lastCartAt + " < " + arg(dayStart(asOf, r.Value.(int)-1))
		case r.Field == FieldPurchasedAfterCart && r.Op == OpEq:
			purchasedAfter := "EXISTS (SELECT 1 FROM user_events p WHERE p.user_id = u.id AND p.type = 'purchase' AND p.created_at > " + lastCartAt + ")"
			if r.Value.(bool) {
				c = purchasedAfter
			} else {
				// Without the IS NOT NULL, users who never added to cart would match.
				c = lastCartAt + " IS NOT NULL AND NOT " + purchasedAfter
			}
		default:
			// Unreachable for a validated segment; guards against the whitelist and the
			// builder drifting apart.
			return "", nil, fmt.Errorf("segment: no SQL mapping for %s %s", r.Field, r.Op)
		}
		conds = append(conds, "("+c+")")
	}
	return strings.Join(conds, " AND "), args, nil
}

// Counts is the reach of a segment: everyone matching it, and those reachable per channel,
// in total and per preferred language (users.language).
type Counts struct {
	Total int `json:"total"`
	SMS   int `json:"sms"`
	Email int `json:"email"`
	// LongestFirstName is the longest first_name in the segment, in characters.
	LongestFirstName int `json:"longest_first_name"`
	// Languages has an entry for each language with anyone in the segment. Content is
	// written per language and its length limits are checked against that language's
	// LongestFirstName (content.Options).
	Languages map[string]LanguageCounts `json:"languages"`
}

// LanguageCounts is the part of a segment that prefers one language.
type LanguageCounts struct {
	Total            int `json:"total"`
	SMS              int `json:"sms"`
	Email            int `json:"email"`
	LongestFirstName int `json:"longest_first_name"`
}

// Reachable reports whether anyone in this part can be reached on some channel.
func (l LanguageCounts) Reachable() bool { return l.SMS+l.Email > 0 }

// ContentLanguages are the languages messages have to be written in: those with at
// least one reachable recipient, in the order of Languages.
func (c Counts) ContentLanguages() []string {
	var out []string
	for _, l := range Languages {
		if c.Languages[l].Reachable() {
			out = append(out, l)
		}
	}
	return out
}

// CountSQL returns one query that computes the reach per language in a single pass over
// the segment: one row per language with anyone in it.
func CountSQL(s Segment, asOf time.Time) (string, []any, error) {
	where, args, err := Build(s, asOf, nil)
	if err != nil {
		return "", nil, err
	}
	q := "SELECT u.language, count(*), " +
		"count(*) FILTER (WHERE " + SMSReachable + "), " +
		"count(*) FILTER (WHERE " + EmailReachable + "), " +
		// char_length counts characters like utf8.RuneCountInString, not bytes.
		"coalesce(max(char_length(u.first_name)), 0) " +
		"FROM users u WHERE " + where + " GROUP BY u.language"
	return q, args, nil
}

// Querier is the subset of pgxpool.Pool / pgx.Tx used here.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Count returns the reach of a validated segment.
func Count(ctx context.Context, db Querier, s Segment, asOf time.Time) (Counts, error) {
	q, args, err := CountSQL(s, asOf)
	if err != nil {
		return Counts{}, err
	}
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return Counts{}, fmt.Errorf("count segment: %w", err)
	}
	defer rows.Close()
	c := Counts{Languages: map[string]LanguageCounts{}}
	for rows.Next() {
		var lang string
		var l LanguageCounts
		if err := rows.Scan(&lang, &l.Total, &l.SMS, &l.Email, &l.LongestFirstName); err != nil {
			return Counts{}, fmt.Errorf("count segment: %w", err)
		}
		c.Languages[lang] = l
		c.Total += l.Total
		c.SMS += l.SMS
		c.Email += l.Email
		c.LongestFirstName = max(c.LongestFirstName, l.LongestFirstName)
	}
	if err := rows.Err(); err != nil {
		return Counts{}, fmt.Errorf("count segment: %w", err)
	}
	return c, nil
}

// KnownCities returns the distinct cities present in the data, for NewValidator and the prompt.
func KnownCities(ctx context.Context, db Querier) ([]string, error) {
	rows, err := db.Query(ctx, "SELECT DISTINCT city FROM users ORDER BY city")
	if err != nil {
		return nil, fmt.Errorf("load cities: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
