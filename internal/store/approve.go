package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denizsahiner/smart-campaign-composer/internal/content"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
)

// ErrNoRecipients means nobody in the segment can be reached on any channel any more.
var ErrNoRecipients = errors.New("store: no reachable recipients")

// ErrMissingLanguage means a recipient prefers a language the approved content doesn't
// have: the audience's languages changed since it was counted.
var ErrMissingLanguage = errors.New("store: a recipient's language has no content")

// Queued summarizes what approval put on the queue.
type Queued struct {
	SMS   int `json:"sms"`
	Email int `json:"email"`
	// WithoutName counts recipients who get the version without their name: empty
	// first_name, or (safety net) a field over its limit with the name.
	WithoutName struct {
		SMS   int `json:"sms"`
		Email int `json:"email"`
	} `json:"without_name"`
	// Recipients per language: how many people get each language's version.
	ByLanguage map[string]int `json:"by_language"`
}

// Approve freezes the campaign with its final content and queues one message per
// (recipient, channel) in the recipient's language, all in one transaction. set has a
// validated draft for every language the audience needs; it replaces the stored drafts.
// sendAt nil sends at once; otherwise the campaign becomes scheduled and every message
// waits for sendAt (next_attempt_at), the recipients and texts still fixed now.
//
//   - The status change is conditional (draft or ready → approved or scheduled). Its row lock makes a
//     concurrent approve wait, then find the campaign approved and get ErrNotEditable:
//     the content can't be overwritten with something other than what was queued.
//   - Recipients are the segment at the stored reference time, with the per-channel
//     consent filter, read inside the transaction.
//   - Messages are rendered here (content.RenderFor, from the recipient's language) and
//     stored rendered, with the address snapshotted, so a row is exactly what gets sent.
//     A recipient whose language has no draft fails the approval (ErrMissingLanguage)
//     rather than getting another language.
//   - ON CONFLICT DO NOTHING on (campaign_id, user_id, channel) is the second layer
//     against duplicates.
//
// With no reachable recipient nothing is written and ErrNoRecipients is returned.
func (s *Store) Approve(ctx context.Context, id int64, set content.Set,
	seg segment.Segment, asOf time.Time, sendAt *time.Time) (Queued, error) {

	q := Queued{ByLanguage: map[string]int{}}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return q, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE campaigns SET status = CASE WHEN $2::timestamptz IS NULL THEN 'approved' ELSE 'scheduled' END,
		       scheduled_at = $2, approved_at = now(), updated_at = now()
		WHERE id = $1 AND status IN ('draft', 'ready')`, id, sendAt)
	if err != nil {
		return q, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return q, err
		}
		return q, ErrNotEditable
	}
	if _, err := tx.Exec(ctx, `DELETE FROM campaign_contents WHERE campaign_id = $1`, id); err != nil {
		return q, err
	}
	for _, l := range set.Languages() {
		c := set[l]
		if _, err := tx.Exec(ctx, `
			INSERT INTO campaign_contents (campaign_id, language, sms, email_subject, email_body)
			VALUES ($1, $2, $3, $4, $5)`, id, l, c.SMS, c.EmailSubject, c.EmailBody); err != nil {
			return q, err
		}
	}

	where, args, err := segment.Build(seg, asOf, nil)
	if err != nil {
		return q, err
	}
	rows, err := tx.Query(ctx, `
		SELECT u.id, u.first_name, u.language,
		       (`+segment.SMSReachable+`), u.phone,
		       (`+segment.EmailReachable+`), u.email
		FROM users u
		WHERE (`+where+`) AND ((`+segment.SMSReachable+`) OR (`+segment.EmailReachable+`))
		ORDER BY u.id`, args...)
	if err != nil {
		return q, fmt.Errorf("read recipients: %w", err)
	}

	var (
		userIDs                 []int64
		channels, addrs, bodies []string
		subjects                []*string
	)
	for rows.Next() {
		var (
			uid          int64
			first, lang  string
			smsOK, emOK  bool
			phone, email *string
		)
		if err := rows.Scan(&uid, &first, &lang, &smsOK, &phone, &emOK, &email); err != nil {
			rows.Close()
			return q, err
		}
		c, ok := set[lang]
		if !ok {
			rows.Close()
			return q, fmt.Errorf("%w: %q (user %d)", ErrMissingLanguage, lang, uid)
		}
		q.ByLanguage[lang]++
		r := content.RenderFor(c, first, lang)
		if smsOK {
			userIDs, channels, addrs, bodies, subjects = append(userIDs, uid), append(channels, "sms"),
				append(addrs, *phone), append(bodies, r.SMS), append(subjects, nil)
			q.SMS++
			if r.SMSWithoutName {
				q.WithoutName.SMS++
			}
		}
		if emOK {
			subject := r.EmailSubject
			userIDs, channels, addrs, bodies, subjects = append(userIDs, uid), append(channels, "email"),
				append(addrs, *email), append(bodies, r.EmailBody), append(subjects, &subject)
			q.Email++
			if r.EmailWithoutName {
				q.WithoutName.Email++
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return q, err
	}
	if len(userIDs) == 0 {
		return Queued{}, ErrNoRecipients
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO messages (campaign_id, user_id, channel, to_address, subject, body, next_attempt_at)
		SELECT $1, m.user_id, m.channel, m.to_address, m.subject, m.body, COALESCE($7::timestamptz, now())
		FROM unnest($2::bigint[], $3::text[], $4::text[], $5::text[], $6::text[])
		     AS m(user_id, channel, to_address, subject, body)
		ON CONFLICT (campaign_id, user_id, channel) DO NOTHING`,
		id, userIDs, channels, addrs, subjects, bodies, sendAt); err != nil {
		return q, fmt.Errorf("queue messages: %w", err)
	}
	return q, tx.Commit(ctx)
}

// ErrNotScheduled means the campaign isn't waiting for a scheduled send (any more).
var ErrNotScheduled = errors.New("store: campaign is not scheduled")

// ErrSendStarted means the scheduled time came and the worker has taken a message: the
// send can't be called off any more.
var ErrSendStarted = errors.New("store: the scheduled send has started")

// CancelSchedule calls off a scheduled send: its queued messages are deleted and the
// campaign goes back to ready, drafts and audience kept, to be edited and approved again.
//
// The messages are deleted first, then the campaign row is updated, the same order the
// worker's claim locks them in (messages, then the campaign it moves to approved), so the
// two can't deadlock. A message the claim got to first is 'sending', isn't deleted, and
// the claim has moved the campaign to approved: the update finds no scheduled campaign and
// everything is rolled back with ErrSendStarted.
func (s *Store) CancelSchedule(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		DELETE FROM messages m USING campaigns c
		WHERE m.campaign_id = $1 AND c.id = $1 AND c.status = 'scheduled' AND m.status = 'pending'`, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE campaigns SET status = 'ready', scheduled_at = NULL, approved_at = NULL, updated_at = now()
		WHERE id = $1 AND status = 'scheduled'
		  AND NOT EXISTS (SELECT 1 FROM messages WHERE campaign_id = $1)`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return tx.Commit(ctx)
	}
	// Why not: a campaign that was scheduled keeps scheduled_at once sending starts.
	c, err := s.Get(ctx, id)
	switch {
	case err != nil:
		return err
	case c.ScheduledAt != nil:
		return ErrSendStarted
	default:
		return ErrNotScheduled
	}
}
