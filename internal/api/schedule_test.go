package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// tomorrowNoon is a send time every rule allows: ahead, within 14 days, outside the
// quiet hours.
func tomorrowNoon() time.Time {
	istanbul := time.FixedZone("UTC+3", 3*60*60)
	d := time.Now().In(istanbul).AddDate(0, 0, 1)
	return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, istanbul)
}

func (e *env) schedule(id int64, content any, sendAt any) (int, map[string]any) {
	e.t.Helper()
	return e.do("POST", path(id, "/approve"), map[string]any{"contents": content, "send_at": sendAt})
}

func TestScheduledSend(t *testing.T) {
	e := newEnv(t)
	id, content := readyCampaign(t, e)
	sendAt := tomorrowNoon()

	status, body := e.schedule(id, content, sendAt.Format(time.RFC3339))
	if status != 200 {
		t.Fatalf("schedule: %d %v", status, body)
	}
	c := body["campaign"].(map[string]any)
	if c["status"] != "scheduled" || c["scheduled_at"] == nil {
		t.Fatalf("campaign = %v", c)
	}
	if got, _ := time.Parse(time.RFC3339, c["scheduled_at"].(string)); !got.Equal(sendAt) {
		t.Errorf("scheduled_at = %v, want %v", c["scheduled_at"], sendAt)
	}
	q := body["queued"].(map[string]any)
	if n := e.count(`SELECT count(*) FROM messages WHERE campaign_id = $1 AND next_attempt_at = $2`, id, sendAt); float64(n) != q["sms"].(float64)+q["email"].(float64) {
		t.Errorf("%d messages due at the scheduled time, queued %v", n, q)
	}

	// Frozen like an approved campaign.
	if status, body := e.approve(id, content); status != 409 || errCode(body) != "already_approved" {
		t.Errorf("approve while scheduled: %d %v", status, body)
	}
	// Its progress can be followed: the stream opens (it would end at completion).
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path(id, "/events"), nil)
	if res, err := e.srv.Client().Do(req); err != nil || res.StatusCode != 200 {
		t.Errorf("events of a scheduled campaign: %v %v", res, err)
	} else {
		res.Body.Close()
	}

	// Cancelled: back to ready with its drafts, nothing queued, approvable again.
	status, c = e.do("POST", path(id, "/schedule/cancel"), nil)
	if status != 200 || c["status"] != "ready" || c["scheduled_at"] != nil || c["contents"] == nil {
		t.Fatalf("cancel: %d %v", status, c)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE campaign_id = $1`, id); n != 0 {
		t.Errorf("%d messages left after cancel", n)
	}
	if status, body := e.do("POST", path(id, "/schedule/cancel"), nil); status != 409 || errCode(body) != "not_scheduled" {
		t.Errorf("second cancel: %d %v", status, body)
	}
	if status, body := e.approve(id, content); status != 200 {
		t.Fatalf("approve after cancel: %d %v", status, body)
	}
}

func TestScheduleRejectsDisallowedTimes(t *testing.T) {
	e := newEnv(t)
	id, content := readyCampaign(t, e)
	night := tomorrowNoon().Add(10 * time.Hour) // 22:00
	for want, sendAt := range map[string]time.Time{
		"in_past":     time.Now().Add(-time.Minute),
		"too_far":     tomorrowNoon().AddDate(0, 0, 14),
		"quiet_hours": night,
	} {
		status, body := e.schedule(id, content, sendAt.Format(time.RFC3339))
		if status != 422 || errCode(body) != "invalid_send_at" || fieldCode(body) != want {
			t.Errorf("%s: %d %v", want, status, body)
		}
	}
	if c := e.get(id); c["status"] != "ready" {
		t.Errorf("status = %v after rejected schedules", c["status"])
	}
}

func TestCancelAnImmediateSend(t *testing.T) {
	e := newEnv(t)
	id, content := readyCampaign(t, e)
	e.approve(id, content)
	if status, body := e.do("POST", path(id, "/schedule/cancel"), nil); status != 409 || errCode(body) != "not_scheduled" {
		t.Errorf("cancel: %d %v", status, body)
	}
}

// fieldCode is the code of the first field error.
func fieldCode(body map[string]any) string {
	errs, _ := body["error"].(map[string]any)["errors"].([]any)
	if len(errs) == 0 {
		return ""
	}
	return errs[0].(map[string]any)["code"].(string)
}
