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
}

// Approve freezes the campaign with its final content and queues one message per
// (recipient, channel), all in one transaction.
//
//   - The status change is conditional (draft or ready → approved). Its row lock makes a
//     concurrent approve wait, then find the campaign approved and get ErrNotEditable:
//     the content can't be overwritten with something other than what was queued.
//   - Recipients are the segment at the stored reference time, with the per-channel
//     consent filter, read inside the transaction.
//   - Messages are rendered here (content.RenderFor) and stored rendered, with the
//     address snapshotted, so a row is exactly what gets sent.
//   - ON CONFLICT DO NOTHING on (campaign_id, user_id, channel) is the second layer
//     against duplicates.
//
// With no reachable recipient nothing is written and ErrNoRecipients is returned.
func (s *Store) Approve(ctx context.Context, id int64, c content.Content, lang string,
	seg segment.Segment, asOf time.Time) (Queued, error) {

	var q Queued
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return q, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE campaigns SET status = 'approved', approved_at = now(), updated_at = now(),
			sms_content = $2, email_subject = $3, email_body = $4, content_errors = NULL,
			content_language = $5
		WHERE id = $1 AND status IN ('draft', 'ready')`,
		id, c.SMS, c.EmailSubject, c.EmailBody, lang)
	if err != nil {
		return q, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return q, err
		}
		return q, ErrNotEditable
	}

	where, args, err := segment.Build(seg, asOf, nil)
	if err != nil {
		return q, err
	}
	rows, err := tx.Query(ctx, `
		SELECT u.id, u.first_name,
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
			first        string
			smsOK, emOK  bool
			phone, email *string
		)
		if err := rows.Scan(&uid, &first, &smsOK, &phone, &emOK, &email); err != nil {
			rows.Close()
			return q, err
		}
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
		INSERT INTO messages (campaign_id, user_id, channel, to_address, subject, body)
		SELECT $1, m.user_id, m.channel, m.to_address, m.subject, m.body
		FROM unnest($2::bigint[], $3::text[], $4::text[], $5::text[], $6::text[])
		     AS m(user_id, channel, to_address, subject, body)
		ON CONFLICT (campaign_id, user_id, channel) DO NOTHING`,
		id, userIDs, channels, addrs, subjects, bodies); err != nil {
		return q, fmt.Errorf("queue messages: %w", err)
	}
	return q, tx.Commit(ctx)
}
