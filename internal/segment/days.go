package segment

import (
	"time"
	_ "time/tzdata" // the distroless runtime image has no zoneinfo; embed it
)

// Location is the calendar that day-based rules are evaluated in. "Last 30 days" means
// calendar days as the marketer sees them, so a day starts at 00:00 Istanbul time, not
// 00:00 UTC (21:00 the previous evening in Istanbul).
var Location = mustLoad("Europe/Istanbul")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// DaysAgo is the definition of cart_added_days_ago: the number of calendar days in
// Location between t and asOf. Today is 0, yesterday is 1, regardless of the hour.
func DaysAgo(asOf, t time.Time) int {
	return int(localDate(asOf).Sub(localDate(t)) / (24 * time.Hour))
}

// dayStart returns 00:00 in Location of the calendar day `days` days before asOf's day.
// time.Date normalizes the day across month and year boundaries (Nov 7 - 30 → Oct 8).
func dayStart(asOf time.Time, days int) time.Time {
	y, m, d := asOf.In(Location).Date()
	return time.Date(y, m, d-days, 0, 0, 0, 0, Location)
}

// localDate maps t to midnight UTC of its calendar date in Location, so subtracting two
// of them gives an exact multiple of 24h.
func localDate(t time.Time) time.Time {
	y, m, d := t.In(Location).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
