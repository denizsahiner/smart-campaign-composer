// Package db holds the schema migrations and the code that applies them.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all pending migrations. Each migration runs in its own transaction.
func Migrate(ctx context.Context, db *sql.DB) error {
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		return fmt.Errorf("init migrations: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		slog.Info("migration applied", "version", r.Source.Version, "duration", r.Duration)
	}
	return nil
}
