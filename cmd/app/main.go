// Command app is the single binary behind the migrate, api and worker containers.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/denizsahiner/smart-campaign-composer/internal/db"
	"github.com/denizsahiner/smart-campaign-composer/internal/seed"
)

const (
	seedUsers = 5000
	seedValue = 42
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: app migrate | api | worker | mock-provider | healthcheck [url] | llm-try \"<brief>\"")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "migrate":
		err = runMigrate(ctx)
	case "api":
		err = runAPI(ctx)
	case "worker":
		err = runWorker(ctx)
	case "mock-provider":
		err = runProvider(ctx)
	case "healthcheck":
		err = runHealthcheck(os.Args[2:])
	case "llm-try":
		err = runLLMTry(ctx, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		slog.Error(os.Args[1]+" failed", "err", err)
		os.Exit(1)
	}
}

// runMigrate applies the schema, then loads the seed if the users table is empty.
func runMigrate(ctx context.Context) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()
	if err := db.Migrate(ctx, sqlDB); err != nil {
		return err
	}

	seeded, err := seed.IsSeeded(ctx, pool)
	if err != nil {
		return fmt.Errorf("check seed: %w", err)
	}
	if seeded {
		slog.Info("seed skipped: users already present")
		return nil
	}
	users, events := seed.Generate(seed.Options{Users: seedUsers, Seed: seedValue, Now: time.Now()})
	if err := seed.Load(ctx, pool, users, events); err != nil {
		return fmt.Errorf("load seed: %w", err)
	}
	slog.Info("seed loaded", "users", len(users), "events", len(events))
	return nil
}
