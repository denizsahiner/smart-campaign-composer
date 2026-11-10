package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denizsahiner/smart-campaign-composer/internal/content"
	"github.com/denizsahiner/smart-campaign-composer/internal/dbtest"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
	"github.com/denizsahiner/smart-campaign-composer/internal/store"
	"github.com/denizsahiner/smart-campaign-composer/internal/validation"
)

func setup(t *testing.T) (*store.Store, *pgxpool.Pool, func(string) int, int64, segment.Segment, time.Time) {
	t.Helper()
	ctx := context.Background()
	pool, _, _ := dbtest.Seeded(t, time.Now())
	st := store.New(pool)
	cities, err := segment.KnownCities(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	seg, err := segment.NewValidator(cities).Parse([]byte(`{"rules":[{"field":"city","op":"eq","value":"Ankara"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	asOf := time.Now()
	counts, err := segment.Count(ctx, pool, seg, asOf)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Create(ctx, "Ankara")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAudience(ctx, id, "Ankara", &seg, counts, asOf, nil); err != nil {
		t.Fatal(err)
	}
	count := func(sql string) int {
		var n int
		if err := pool.QueryRow(ctx, sql, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return st, pool, count, id, seg, asOf
}

var first = content.Set{
	"tr": {SMS: "Merhaba {{first_name}}!", EmailSubject: "Konu", EmailBody: "Merhaba {{first_name}},"},
	"en": {SMS: "Hi {{first_name}}!", EmailSubject: "Subject", EmailBody: "Hi {{first_name}},"},
}

// The conditional status change is what stops a second approve from overwriting the
// queued content, whatever the API layer in front of it checks.
func TestApproveTwiceAtStoreLevel(t *testing.T) {
	ctx := context.Background()
	st, _, count, id, seg, asOf := setup(t)

	q, err := st.Approve(ctx, id, first, seg, asOf, nil)
	if err != nil || q.SMS == 0 || q.Email == 0 {
		t.Fatalf("first approve: %+v, %v", q, err)
	}
	queued := count(`SELECT count(*) FROM messages WHERE campaign_id = $1`)

	other := content.Content{SMS: "Başka", EmailSubject: "Başka", EmailBody: "Başka"}
	if _, err := st.Approve(ctx, id, content.Set{"tr": other, "en": other}, seg, asOf, nil); !errors.Is(err, store.ErrNotEditable) {
		t.Fatalf("second approve: err = %v, want ErrNotEditable", err)
	}
	c, _ := st.Get(ctx, id)
	if c.Contents["tr"].SMS != first["tr"].SMS || c.Status != store.StatusApproved {
		t.Errorf("approved content overwritten: %+v", c.Contents)
	}
	if n := count(`SELECT count(*) FROM messages WHERE campaign_id = $1`); n != queued {
		t.Errorf("messages %d → %d", queued, n)
	}
	if n := count(`SELECT count(*) FROM messages WHERE campaign_id = $1 AND body = 'Başka'`); n != 0 {
		t.Errorf("%d messages with the second content", n)
	}
}

// No reachable recipient: the whole transaction rolls back, status included.
func TestApproveNoRecipientsRollsBack(t *testing.T) {
	ctx := context.Background()
	st, pool, _, id, seg, asOf := setup(t)
	tag, err := pool.Exec(ctx, `UPDATE users SET sms_consent = false, email_consent = false WHERE city = 'Ankara'`)
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("no Ankara users to update: %v", err)
	}
	if _, err := st.Approve(ctx, id, first, seg, asOf, nil); !errors.Is(err, store.ErrNoRecipients) {
		t.Fatalf("err = %v, want ErrNoRecipients", err)
	}
	c, _ := st.Get(ctx, id)
	if c.Status != store.StatusDraft || c.ApprovedAt != nil || c.Contents != nil {
		t.Errorf("campaign changed by a rolled-back approve: %+v", c)
	}
}

// Each recipient gets their own language's version, and the stored drafts become exactly
// the approved set.
func TestApproveRendersEachRecipientsLanguage(t *testing.T) {
	ctx := context.Background()
	st, pool, count, id, seg, asOf := setup(t)
	c, _ := st.Get(ctx, id)
	if got := c.ContentLanguages(); len(got) != 2 {
		t.Fatalf("Ankara audience languages = %v, want tr and en (the seed has both)", got)
	}
	q, err := st.Approve(ctx, id, first, seg, asOf, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrong := count(`SELECT count(*) FROM messages m JOIN users u ON u.id = m.user_id
		WHERE m.campaign_id = $1 AND m.channel = 'sms' AND
		      ((u.language = 'tr' AND m.body NOT LIKE 'Merhaba%') OR (u.language = 'en' AND m.body NOT LIKE 'Hi%'))`)
	if wrong != 0 {
		t.Errorf("%d SMS in the wrong language", wrong)
	}
	var en int
	pool.QueryRow(ctx, `SELECT count(DISTINCT m.user_id) FROM messages m JOIN users u ON u.id = m.user_id
		WHERE m.campaign_id = $1 AND u.language = 'en'`, id).Scan(&en)
	if q.ByLanguage["en"] != en || q.ByLanguage["en"] == 0 || q.ByLanguage["tr"] == 0 {
		t.Errorf("by_language = %v, %d English recipients queued", q.ByLanguage, en)
	}
}

func TestApproveWithoutARecipientsLanguageFails(t *testing.T) {
	ctx := context.Background()
	st, _, count, id, seg, asOf := setup(t)
	if _, err := st.Approve(ctx, id, content.Set{"tr": first["tr"]}, seg, asOf, nil); !errors.Is(err, store.ErrMissingLanguage) {
		t.Fatal("approved with English recipients and no English content")
	}
	if n := count(`SELECT count(*) FROM messages WHERE campaign_id = $1`); n != 0 {
		t.Errorf("%d messages queued by a failed approve", n)
	}
}

// Ready needs a valid draft in every language the audience has; drafts are stored per
// language and kept across SetContents calls for other languages.
func TestSetContentsReadiness(t *testing.T) {
	ctx := context.Background()
	st, _, _, id, _, _ := setup(t)
	status := func() string {
		c, err := st.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return c.Status
	}
	if err := st.SetContents(ctx, id, content.Set{"tr": first["tr"]}, nil); err != nil {
		t.Fatal(err)
	}
	if s := status(); s != store.StatusDraft {
		t.Errorf("only Turkish: %s, want draft", s)
	}
	bad := []validation.FieldError{{Path: "en.sms", Code: "too_long", Message: "x"}}
	if err := st.SetContents(ctx, id, content.Set{"en": first["en"]}, bad); err != nil {
		t.Fatal(err)
	}
	c, _ := st.Get(ctx, id)
	if c.Status != store.StatusDraft || len(c.Contents) != 2 || len(c.ContentErrors) != 1 || c.ContentErrors[0].Path != "en.sms" {
		t.Errorf("invalid English: %+v", c)
	}
	if err := st.SetContents(ctx, id, content.Set{"en": first["en"]}, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.Get(ctx, id); c.Status != store.StatusReady || c.ContentErrors != nil || c.Contents["tr"] != first["tr"] {
		t.Errorf("both valid: %+v", c)
	}
}

// A scheduled approve queues everything now, each message due at the scheduled time;
// cancelling deletes them and gives back a ready campaign that can be approved again.
func TestScheduleAndCancel(t *testing.T) {
	ctx := context.Background()
	st, pool, count, id, seg, asOf := setup(t)
	sendAt := time.Now().Add(2 * time.Hour).Truncate(time.Microsecond)

	q, err := st.Approve(ctx, id, first, seg, asOf, &sendAt)
	if err != nil || q.SMS == 0 {
		t.Fatalf("approve: %+v, %v", q, err)
	}
	c, _ := st.Get(ctx, id)
	if c.Status != store.StatusScheduled || c.ScheduledAt == nil || !c.ScheduledAt.Equal(sendAt) {
		t.Fatalf("campaign = %s at %v, want scheduled at %v", c.Status, c.ScheduledAt, sendAt)
	}
	if n := count(`SELECT count(*) FROM messages WHERE campaign_id = $1 AND next_attempt_at <> (SELECT scheduled_at FROM campaigns WHERE id = $1)`); n != 0 {
		t.Errorf("%d messages not due at the scheduled time", n)
	}

	if err := st.CancelSchedule(ctx, id); err != nil {
		t.Fatal(err)
	}
	c, _ = st.Get(ctx, id)
	if c.Status != store.StatusReady || c.ScheduledAt != nil || c.ApprovedAt != nil || c.Contents["en"] != first["en"] {
		t.Errorf("after cancel: %+v", c)
	}
	if n := count(`SELECT count(*) FROM messages WHERE campaign_id = $1`); n != 0 {
		t.Errorf("%d messages left after cancel", n)
	}
	if err := st.CancelSchedule(ctx, id); !errors.Is(err, store.ErrNotScheduled) {
		t.Errorf("second cancel: err = %v, want ErrNotScheduled", err)
	}

	// Approved again, then the send starts (a message is taken, as the worker's claim
	// does it): too late to cancel, and nothing is deleted.
	if _, err := st.Approve(ctx, id, first, seg, asOf, &sendAt); err != nil {
		t.Fatal(err)
	}
	queued := count(`SELECT count(*) FROM messages WHERE campaign_id = $1`)
	pool.Exec(ctx, `UPDATE messages SET status = 'sending', locked_until = now() + interval '1 minute'
		WHERE id = (SELECT min(id) FROM messages WHERE campaign_id = $1)`, id)
	pool.Exec(ctx, `UPDATE campaigns SET status = 'approved' WHERE id = $1`, id)
	if err := st.CancelSchedule(ctx, id); !errors.Is(err, store.ErrSendStarted) {
		t.Errorf("cancel after start: err = %v, want ErrSendStarted", err)
	}
	if n := count(`SELECT count(*) FROM messages WHERE campaign_id = $1`); n != queued {
		t.Errorf("messages %d → %d after a refused cancel", queued, n)
	}
}

func TestCancelAnImmediateSendIsRefused(t *testing.T) {
	ctx := context.Background()
	st, _, _, id, seg, asOf := setup(t)
	if _, err := st.Approve(ctx, id, first, seg, asOf, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CancelSchedule(ctx, id); !errors.Is(err, store.ErrNotScheduled) {
		t.Errorf("err = %v, want ErrNotScheduled", err)
	}
}
