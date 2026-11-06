package seed

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IsSeeded reports whether users already exist, so re-running migrate doesn't load twice.
func IsSeeded(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&exists)
	return exists, err
}

// Load bulk-inserts users and events with COPY in a single transaction:
// either everything is written or nothing is.
func Load(ctx context.Context, pool *pgxpool.Pool, users []User, events []Event) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	userRows := make([][]any, len(users))
	for i, u := range users {
		userRows[i] = []any{u.ID, u.FirstName, u.LastName, u.Email, u.Phone, u.City,
			u.Language, u.SMSConsent, u.EmailConsent, u.CreatedAt}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"users"},
		[]string{"id", "first_name", "last_name", "email", "phone", "city",
			"language", "sms_consent", "email_consent", "created_at"},
		pgx.CopyFromRows(userRows)); err != nil {
		return fmt.Errorf("copy users: %w", err)
	}

	eventRows := make([][]any, len(events))
	for i, e := range events {
		eventRows[i] = []any{e.UserID, e.Type, e.CreatedAt}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"user_events"},
		[]string{"user_id", "type", "created_at"},
		pgx.CopyFromRows(eventRows)); err != nil {
		return fmt.Errorf("copy user_events: %w", err)
	}

	// IDs were inserted explicitly; move the identity sequence past them.
	if _, err := tx.Exec(ctx,
		`SELECT setval(pg_get_serial_sequence('users', 'id'), (SELECT max(id) FROM users))`); err != nil {
		return fmt.Errorf("reset users id sequence: %w", err)
	}

	return tx.Commit(ctx)
}
