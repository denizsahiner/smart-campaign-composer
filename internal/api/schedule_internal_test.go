package api

import (
	"testing"
	"time"
)

func TestCheckSendAt(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, istanbul)
	at := func(day, hour, min int) time.Time { return time.Date(2026, 11, day, hour, min, 0, 0, istanbul) }
	for name, tc := range map[string]struct {
		sendAt time.Time
		want   string
	}{
		"in a minute":        {now.Add(time.Minute), ""},
		"under a minute":     {now.Add(59 * time.Second), "in_past"},
		"in the past":        {now.Add(-time.Hour), "in_past"},
		"09:00 opens":        {at(11, 9, 0), ""},
		"20:59 is the last":  {at(11, 20, 59), ""},
		"21:00 is quiet":     {at(11, 21, 0), "quiet_hours"},
		"08:59 is quiet":     {at(11, 8, 59), "quiet_hours"},
		"midnight is quiet":  {at(12, 0, 0), "quiet_hours"},
		"quiet in UTC terms": {time.Date(2026, 11, 11, 19, 0, 0, 0, time.UTC), "quiet_hours"}, // 22:00 Istanbul
		"exactly 14 days":    {now.Add(14 * 24 * time.Hour), ""},
		"just over 14 days":  {now.Add(14*24*time.Hour + time.Minute), "too_far"},
	} {
		t.Run(name, func(t *testing.T) {
			errs := checkSendAt(now, tc.sendAt)
			got := ""
			if len(errs) > 0 {
				got = errs[0].Code
				if errs[0].Path != "send_at" {
					t.Errorf("path = %s", errs[0].Path)
				}
			}
			if got != tc.want {
				t.Errorf("checkSendAt(%v) = %q, want %q", tc.sendAt, got, tc.want)
			}
		})
	}
}
