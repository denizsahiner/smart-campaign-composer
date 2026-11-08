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

var first = content.Content{SMS: "Merhaba {{first_name}}!", EmailSubject: "Konu", EmailBody: "Merhaba {{first_name}},"}

// The conditional status change is what stops a second approve from overwriting the
// queued content, whatever the API layer in front of it checks.
func TestApproveTwiceAtStoreLevel(t *testing.T) {
	ctx := context.Background()
	st, _, count, id, seg, asOf := setup(t)

	q, err := st.Approve(ctx, id, first, "tr", seg, asOf)
	if err != nil || q.SMS == 0 || q.Email == 0 {
		t.Fatalf("first approve: %+v, %v", q, err)
	}
	queued := count(`SELECT count(*) FROM messages WHERE campaign_id = $1`)

	other := content.Content{SMS: "Başka", EmailSubject: "Başka", EmailBody: "Başka"}
	if _, err := st.Approve(ctx, id, other, "tr", seg, asOf); !errors.Is(err, store.ErrNotEditable) {
		t.Fatalf("second approve: err = %v, want ErrNotEditable", err)
	}
	c, _ := st.Get(ctx, id)
	if c.Content.SMS != first.SMS || c.Status != store.StatusApproved {
		t.Errorf("approved content overwritten: %+v", c.Content)
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
	if _, err := st.Approve(ctx, id, first, "tr", seg, asOf); !errors.Is(err, store.ErrNoRecipients) {
		t.Fatalf("err = %v, want ErrNoRecipients", err)
	}
	c, _ := st.Get(ctx, id)
	if c.Status != store.StatusDraft || c.ApprovedAt != nil || c.Content != nil {
		t.Errorf("campaign changed by a rolled-back approve: %+v", c)
	}
}
