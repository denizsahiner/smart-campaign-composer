package segment

import (
	"testing"
	"time"
)

func TestDaysAgo(t *testing.T) {
	asOf := istanbul(2026, 11, 7, 14, 0, 0)
	cases := []struct {
		name string
		t    time.Time
		want int
	}{
		{"same instant", asOf, 0},
		{"today 00:00", istanbul(2026, 11, 7, 0, 0, 0), 0},
		{"later today", istanbul(2026, 11, 7, 23, 59, 59), 0},
		{"yesterday 23:59:59", istanbul(2026, 11, 6, 23, 59, 59), 1},
		{"30 days back, 00:00", istanbul(2026, 10, 8, 0, 0, 0), 30},
		{"30 days back, 23:59:59", istanbul(2026, 10, 8, 23, 59, 59), 30},
		{"31 days back, 23:59:59", istanbul(2026, 10, 7, 23, 59, 59), 31},
		// The same instants written in UTC: Istanbul is UTC+3, so the Istanbul day
		// starts at 21:00 UTC the evening before.
		{"UTC 21:00 = Istanbul 00:00", time.Date(2026, 10, 7, 21, 0, 0, 0, time.UTC), 30},
		{"UTC 20:59:59 = Istanbul 23:59:59", time.Date(2026, 10, 7, 20, 59, 59, 0, time.UTC), 31},
	}
	for _, c := range cases {
		if got := DaysAgo(asOf, c.t); got != c.want {
			t.Errorf("%s: DaysAgo = %d, want %d", c.name, got, c.want)
		}
	}
	// asOf late in the evening in UTC terms is already the next day in Istanbul.
	lateUTC := time.Date(2026, 11, 7, 22, 30, 0, 0, time.UTC) // Nov 8 01:30 Istanbul
	if got := DaysAgo(lateUTC, istanbul(2026, 11, 7, 12, 0, 0)); got != 1 {
		t.Errorf("asOf after Istanbul midnight: DaysAgo = %d, want 1", got)
	}
}

func TestDayStart(t *testing.T) {
	cases := []struct {
		asOf time.Time
		days int
		want time.Time
	}{
		{istanbul(2026, 11, 7, 14, 0, 0), 0, istanbul(2026, 11, 7, 0, 0, 0)},
		{istanbul(2026, 11, 7, 14, 0, 0), 30, istanbul(2026, 10, 8, 0, 0, 0)},
		{istanbul(2026, 3, 1, 9, 0, 0), 1, istanbul(2026, 2, 28, 0, 0, 0)},   // month boundary
		{istanbul(2028, 3, 1, 9, 0, 0), 1, istanbul(2028, 2, 29, 0, 0, 0)},   // leap year
		{istanbul(2026, 1, 5, 9, 0, 0), 10, istanbul(2025, 12, 26, 0, 0, 0)}, // year boundary
		{istanbul(2026, 11, 7, 14, 0, 0), 365, istanbul(2025, 11, 7, 0, 0, 0)},
		// asOf given in UTC is converted to Istanbul before taking the date.
		{time.Date(2026, 11, 7, 22, 30, 0, 0, time.UTC), 0, istanbul(2026, 11, 8, 0, 0, 0)},
	}
	for _, c := range cases {
		if got := dayStart(c.asOf, c.days); !got.Equal(c.want) {
			t.Errorf("dayStart(%v, %d) = %v, want %v", c.asOf, c.days, got, c.want)
		}
	}
}

// The SQL boundaries (dayStart) and the definition (DaysAgo) are two formulations of the
// same rule; check they agree just before, at and after every boundary for n in 1–365.
func TestDayStartAgreesWithDaysAgo(t *testing.T) {
	asOf := istanbul(2026, 11, 7, 14, 0, 0)
	for n := MinDays; n <= MaxDays; n++ {
		lteStart := dayStart(asOf, n) // lte n: t >= lteStart
		gteEnd := dayStart(asOf, n-1) // gte n: t < gteEnd
		for _, d := range []time.Duration{-time.Second, -time.Microsecond, 0, time.Microsecond, time.Second} {
			at := lteStart.Add(d)
			if (DaysAgo(asOf, at) <= n) != !at.Before(lteStart) {
				t.Fatalf("lte %d disagrees at %v", n, at)
			}
			at = gteEnd.Add(d)
			if (DaysAgo(asOf, at) >= n) != at.Before(gteEnd) {
				t.Fatalf("gte %d disagrees at %v", n, at)
			}
		}
	}
}
