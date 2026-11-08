package segment

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denizsahiner/smart-campaign-composer/internal/dbtest"
	"github.com/denizsahiner/smart-campaign-composer/internal/seed"
)

func matchedIDs(t *testing.T, pool *pgxpool.Pool, s Segment, asOf time.Time) map[int64]bool {
	t.Helper()
	where, args, err := Build(s, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := pool.Query(context.Background(), "SELECT u.id FROM users u WHERE "+where, args...)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, id := range ids {
		got[id] = true
	}
	return got
}

// Exact boundaries, with hand-placed events. asOf is Nov 7 2026 14:00 Istanbul, so
// "lte 30" starts at Oct 8 00:00 Istanbul = Oct 7 21:00 UTC.
func TestSegmentSQLBoundaries(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	asOf := istanbul(2026, 11, 7, 14, 0, 0)
	µs := time.Microsecond // Postgres timestamptz precision

	type ev struct {
		typ string
		at  time.Time
	}
	users := []struct {
		name   string
		events []ev
	}{
		{"day30_first_instant", []ev{{"cart_add", istanbul(2026, 10, 8, 0, 0, 0)}}},
		{"day31_last_instant", []ev{{"cart_add", istanbul(2026, 10, 8, 0, 0, 0).Add(-µs)}}},
		// UTC date is Oct 7, Istanbul date is Oct 8: must count as day 30, not 31.
		{"utc_oct7_istanbul_oct8", []ev{{"cart_add", time.Date(2026, 10, 7, 22, 30, 0, 0, time.UTC)}}},
		{"today_before_asof", []ev{{"cart_add", istanbul(2026, 11, 7, 13, 59, 0)}}},
		{"today_after_asof", []ev{{"cart_add", istanbul(2026, 11, 7, 15, 0, 0)}}},
		{"yesterday_last_instant", []ev{{"cart_add", istanbul(2026, 11, 7, 0, 0, 0).Add(-µs)}}},
		// Only the most recent cart_add counts.
		{"old_and_recent_cart", []ev{{"cart_add", istanbul(2026, 9, 1, 10, 0, 0)}, {"cart_add", istanbul(2026, 10, 20, 10, 0, 0)}}},
		// "After" is strict: a purchase at the same instant doesn't count.
		{"purchase_same_instant", []ev{{"cart_add", istanbul(2026, 10, 20, 10, 0, 0)}, {"purchase", istanbul(2026, 10, 20, 10, 0, 0)}}},
		{"purchase_1us_after", []ev{{"cart_add", istanbul(2026, 10, 20, 10, 0, 0)}, {"purchase", istanbul(2026, 10, 20, 10, 0, 0).Add(µs)}}},
		{"purchase_only", []ev{{"purchase", istanbul(2026, 10, 20, 10, 0, 0)}}},
		{"no_events", nil},
	}
	idOf := map[string]int64{}
	for i, u := range users {
		id := int64(i + 1)
		idOf[u.name] = id
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, first_name, last_name, city, language) VALUES ($1, $2, '', 'İstanbul', 'tr')`, id, u.name); err != nil {
			t.Fatal(err)
		}
		for _, e := range u.events {
			if _, err := pool.Exec(ctx, `INSERT INTO user_events (user_id, type, created_at) VALUES ($1, $2, $3)`, id, e.typ, e.at); err != nil {
				t.Fatal(err)
			}
		}
	}

	v := NewValidator([]string{"İstanbul"})
	cases := []struct {
		rules string
		want  []string
	}{
		{`{"field":"cart_added_days_ago","op":"lte","value":30}`, []string{
			"day30_first_instant", "utc_oct7_istanbul_oct8", "today_before_asof", "today_after_asof",
			"yesterday_last_instant", "old_and_recent_cart", "purchase_same_instant", "purchase_1us_after"}},
		{`{"field":"cart_added_days_ago","op":"gte","value":31}`, []string{"day31_last_instant"}},
		{`{"field":"cart_added_days_ago","op":"gte","value":30},{"field":"cart_added_days_ago","op":"lte","value":30}`, []string{
			"day30_first_instant", "utc_oct7_istanbul_oct8"}},
		{`{"field":"cart_added_days_ago","op":"lte","value":1}`, []string{
			"today_before_asof", "today_after_asof", "yesterday_last_instant"}},
		{`{"field":"cart_added_days_ago","op":"gte","value":1},{"field":"cart_added_days_ago","op":"lte","value":1}`, []string{
			"yesterday_last_instant"}},
		{`{"field":"purchased_after_cart","op":"eq","value":false}`, []string{
			"day30_first_instant", "day31_last_instant", "utc_oct7_istanbul_oct8", "today_before_asof",
			"today_after_asof", "yesterday_last_instant", "old_and_recent_cart", "purchase_same_instant"}},
		{`{"field":"purchased_after_cart","op":"eq","value":true}`, []string{"purchase_1us_after"}},
	}
	for _, c := range cases {
		s, err := v.Parse([]byte(`{"rules":[` + c.rules + `]}`))
		if err != nil {
			t.Fatal(err)
		}
		want := map[int64]bool{}
		for _, n := range c.want {
			want[idOf[n]] = true
		}
		if got := matchedIDs(t, pool, s, asOf); !maps.Equal(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", c.rules, names(got, idOf), c.want)
		}
	}
}

func names(ids map[int64]bool, idOf map[string]int64) []string {
	var out []string
	for n, id := range idOf {
		if ids[id] {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// The SQL must select exactly the users the Go reference implementation of the DSL
// semantics (DaysAgo) selects, across the whole seed, including the fixed edge cases.
func TestSegmentSQLMatchesReference(t *testing.T) {
	ctx := context.Background()
	asOf := time.Now()
	pool, users, events := dbtest.Seeded(t, asOf)

	cities, err := KnownCities(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	v := NewValidator(cities)
	days := func(lc *time.Time) int { return DaysAgo(asOf, *lc) }

	cases := []struct {
		name string
		raw  string
		ref  func(u seed.User, lastCart *time.Time, boughtAfter bool) bool
	}{
		{
			"example brief",
			`{"rules":[{"field":"city","op":"eq","value":"istanbul"},{"field":"cart_added_days_ago","op":"lte","value":30},{"field":"purchased_after_cart","op":"eq","value":false}]}`,
			func(u seed.User, lc *time.Time, bought bool) bool {
				return u.City == "İstanbul" && lc != nil && days(lc) <= 30 && !bought
			},
		},
		{
			"stale carts, bought after",
			`{"rules":[{"field":"cart_added_days_ago","op":"gte","value":31},{"field":"purchased_after_cart","op":"eq","value":true}]}`,
			func(u seed.User, lc *time.Time, bought bool) bool {
				return lc != nil && days(lc) >= 31 && bought
			},
		},
		{
			"window lte 10",
			`{"rules":[{"field":"cart_added_days_ago","op":"lte","value":10}]}`,
			func(u seed.User, lc *time.Time, _ bool) bool { return lc != nil && days(lc) <= 10 },
		},
		{
			"window 45-90",
			`{"rules":[{"field":"cart_added_days_ago","op":"gte","value":45},{"field":"cart_added_days_ago","op":"lte","value":90}]}`,
			func(u seed.User, lc *time.Time, _ bool) bool {
				return lc != nil && days(lc) >= 45 && days(lc) <= 90
			},
		},
		{
			// Alone, so users who never added to cart (40% of the seed) must stay out.
			"not purchased, alone",
			`{"rules":[{"field":"purchased_after_cart","op":"eq","value":false}]}`,
			func(u seed.User, lc *time.Time, bought bool) bool { return lc != nil && !bought },
		},
		{
			"cities and language",
			`{"rules":[{"field":"city","op":"in","value":["ankara","IZMIR"]},{"field":"language","op":"eq","value":"en"}]}`,
			func(u seed.User, _ *time.Time, _ bool) bool {
				return (u.City == "Ankara" || u.City == "İzmir") && u.Language == "en"
			},
		},
	}

	lastCart := map[int64]time.Time{}
	for _, e := range events {
		if e.Type == seed.EventCartAdd && e.CreatedAt.After(lastCart[e.UserID]) {
			lastCart[e.UserID] = e.CreatedAt
		}
	}
	boughtAfter := map[int64]bool{}
	for _, e := range events {
		if c, ok := lastCart[e.UserID]; ok && e.Type == seed.EventPurchase && e.CreatedAt.After(c) {
			boughtAfter[e.UserID] = true
		}
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := v.Parse([]byte(c.raw))
			if err != nil {
				t.Fatal(err)
			}
			want := map[int64]bool{}
			wantSMS, wantEmail, wantLongest := 0, 0, 0
			for _, u := range users {
				var lc *time.Time
				if at, ok := lastCart[u.ID]; ok {
					lc = &at
				}
				if c.ref(u, lc, boughtAfter[u.ID]) {
					want[u.ID] = true
					wantLongest = max(wantLongest, utf8.RuneCountInString(u.FirstName))
					if u.SMSConsent && u.Phone != nil {
						wantSMS++
					}
					if u.EmailConsent && u.Email != nil {
						wantEmail++
					}
				}
			}
			if len(want) == 0 {
				t.Fatal("reference matched nobody; the case doesn't test anything")
			}

			if got := matchedIDs(t, pool, s, asOf); !maps.Equal(got, want) {
				t.Fatalf("SQL matched %d users, reference %d; only in SQL: %v, only in reference: %v",
					len(got), len(want), diff(got, want), diff(want, got))
			}
			counts, err := Count(ctx, pool, s, asOf)
			if err != nil {
				t.Fatal(err)
			}
			if wantC := (Counts{len(want), wantSMS, wantEmail, wantLongest}); counts != wantC {
				t.Fatalf("counts = %+v, want %+v", counts, wantC)
			}
		})
	}
}

func diff(a, b map[int64]bool) []int64 {
	var out []int64
	for id := range a {
		if !b[id] {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
