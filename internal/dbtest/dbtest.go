// Package dbtest gives integration tests a throwaway, migrated Postgres database.
// `make test` starts the compose Postgres and sets TEST_DATABASE_URL. When it is unset
// (plain `go test`), tests that need a database are skipped.
package dbtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/denizsahiner/smart-campaign-composer/internal/db"
	"github.com/denizsahiner/smart-campaign-composer/internal/seed"
)

// New creates a database on TEST_DATABASE_URL, applies the migrations and drops the
// database when the test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(ctx) })

	name := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
	})

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()
	if err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	return pool
}

// Seeded is New plus the standard seed (5,000 users, fixed random seed) relative to now.
func Seeded(t testing.TB, now time.Time) (*pgxpool.Pool, []seed.User, []seed.Event) {
	t.Helper()
	pool := New(t)
	users, events := seed.Generate(seed.Options{Users: 5000, Seed: 42, Now: now})
	if err := seed.Load(context.Background(), pool, users, events); err != nil {
		t.Fatal(err)
	}
	return pool, users, events
}
