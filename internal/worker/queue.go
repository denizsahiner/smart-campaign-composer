package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Message is a claimed row. Attempts is the value after the claim's increment and is the
// fencing token for the result write.
type Message struct {
	ID         int64
	CampaignID int64
	Channel    string
	To         string
	Subject    *string
	Body       string
	Attempts   int
}

// Queue is the messages table used as a queue. All times are the database's clock.
type Queue struct{ pool *pgxpool.Pool }

func NewQueue(pool *pgxpool.Pool) *Queue { return &Queue{pool: pool} }

const claimSQL = `
UPDATE messages SET status = 'sending', locked_until = now() + $3 * interval '1 millisecond',
                    attempts = attempts + 1, updated_at = now()
WHERE id IN (
  SELECT id FROM messages
  WHERE channel = $1 AND %s
  ORDER BY %s
  LIMIT $2
  FOR UPDATE SKIP LOCKED
)
RETURNING id, campaign_id, channel, to_address, subject, body, attempts`

var (
	// Kept as two queries instead of one OR, so each uses its own partial index.
	claimDue = fmt.Sprintf(claimSQL,
		`status IN ('pending', 'retry_scheduled') AND next_attempt_at <= now()`, `next_attempt_at`)
	claimExpired = fmt.Sprintf(claimSQL,
		`status = 'sending' AND locked_until < now()`, `locked_until`)
)

// Claim leases up to limit rows of a channel: first rows whose lease expired (a worker
// died mid-send), then rows that are due.
func (q *Queue) Claim(ctx context.Context, channel string, limit int, lease time.Duration) ([]Message, error) {
	expired, err := q.claim(ctx, claimExpired, channel, limit, lease)
	if err != nil || len(expired) == limit {
		return expired, err
	}
	due, err := q.claim(ctx, claimDue, channel, limit-len(expired), lease)
	return append(expired, due...), err
}

func (q *Queue) claim(ctx context.Context, sql, channel string, limit int, lease time.Duration) ([]Message, error) {
	rows, err := q.pool.Query(ctx, sql, channel, limit, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Message, error) {
		var m Message
		err := r.Scan(&m.ID, &m.CampaignID, &m.Channel, &m.To, &m.Subject, &m.Body, &m.Attempts)
		return m, err
	})
}

// The result writes are fenced on (status = 'sending', attempts = the claimed value): if
// the lease expired and the row was claimed again, attempts moved on and a late result
// updates nothing instead of overwriting the newer outcome.

// MarkSent records a delivery. Reports false when the fence rejected the write.
func (q *Queue) MarkSent(ctx context.Context, m Message, providerID string) (bool, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE messages SET status = 'sent', provider_message_id = $3, sent_at = now(),
		       locked_until = NULL, last_error = NULL, updated_at = now()
		WHERE id = $1 AND status = 'sending' AND attempts = $2`, m.ID, m.Attempts, providerID)
	return tag.RowsAffected() == 1, err
}

// MarkFailed ends the message without another attempt.
func (q *Queue) MarkFailed(ctx context.Context, m Message, lastErr string) (bool, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE messages SET status = 'failed', locked_until = NULL, last_error = $3, updated_at = now()
		WHERE id = $1 AND status = 'sending' AND attempts = $2`, m.ID, m.Attempts, lastErr)
	return tag.RowsAffected() == 1, err
}

// Reschedule puts the message back for another attempt after delay. refund gives the
// claimed attempt back (429, unreachable provider, a paused channel handing rows back).
func (q *Queue) Reschedule(ctx context.Context, m Message, delay time.Duration, lastErr string, refund bool) (bool, error) {
	dec := 0
	if refund {
		dec = 1
	}
	var errArg *string
	if lastErr != "" {
		errArg = &lastErr
	}
	tag, err := q.pool.Exec(ctx, `
		UPDATE messages SET status = 'retry_scheduled',
		       next_attempt_at = now() + $3 * interval '1 millisecond',
		       attempts = attempts - $4, locked_until = NULL,
		       last_error = COALESCE($5, last_error), updated_at = now()
		WHERE id = $1 AND status = 'sending' AND attempts = $2`,
		m.ID, m.Attempts, delay.Milliseconds(), dec, errArg)
	return tag.RowsAffected() == 1, err
}

// CompleteCampaigns marks approved campaigns completed once none of their messages is
// waiting for work.
func (q *Queue) CompleteCampaigns(ctx context.Context, ids []int64) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE campaigns c SET status = 'completed', updated_at = now()
		WHERE c.id = ANY($1) AND c.status = 'approved'
		  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.campaign_id = c.id
		                  AND m.status IN ('pending', 'sending', 'retry_scheduled'))`, ids)
	return err
}

func (q *Queue) Ping(ctx context.Context) error { return q.pool.Ping(ctx) }
