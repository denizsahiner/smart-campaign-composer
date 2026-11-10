package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denizsahiner/smart-campaign-composer/internal/worker"
)

func workerConfigFromEnv() (worker.Config, error) {
	var e envError
	d := worker.DefaultConfig()
	cfg := worker.Config{
		RatePerChannel: e.float("WORKER_RATE_PER_CHANNEL", d.RatePerChannel),
		Concurrency:    e.int("WORKER_CONCURRENCY_PER_CHANNEL", d.Concurrency),
		BatchSize:      e.int("WORKER_BATCH_SIZE", d.BatchSize),
		Lease:          e.duration("WORKER_LEASE", d.Lease),
		MaxAttempts:    e.int("WORKER_MAX_ATTEMPTS", d.MaxAttempts),
		BackoffBase:    e.duration("WORKER_BACKOFF_BASE", d.BackoffBase),
		PollInterval:   e.duration("WORKER_POLL_INTERVAL", d.PollInterval),
	}
	if err := e.err(); err != nil {
		return cfg, err
	}
	if cfg.RatePerChannel <= 0 || cfg.Concurrency < 1 || cfg.BatchSize < 1 || cfg.MaxAttempts < 1 ||
		cfg.Lease <= 0 || cfg.PollInterval <= 0 {
		return cfg, errors.New("worker settings must be positive")
	}
	return cfg, nil
}

// runWorker sends queued messages until ctx is cancelled (SIGTERM).
func runWorker(ctx context.Context) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	cfg, err := workerConfigFromEnv()
	if err != nil {
		return err
	}
	var e envError
	httpTimeout := e.duration("WORKER_HTTP_TIMEOUT", 3*time.Second)
	if err := e.err(); err != nil {
		return err
	}
	if cfg.Lease <= httpTimeout {
		return fmt.Errorf("WORKER_LEASE (%v) must exceed WORKER_HTTP_TIMEOUT (%v)", cfg.Lease, httpTimeout)
	}
	providerURL := envString("PROVIDER_URL", "http://localhost:8090")

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	defer withLogSink(pool)()

	w := worker.New(cfg, worker.NewQueue(pool), worker.NewHTTPSender(providerURL, httpTimeout))
	health := &http.Server{
		Addr:              envString("WORKER_HEALTH_ADDR", ":8081"),
		Handler:           w.HealthHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("worker health server", "err", err)
		}
	}()
	slog.Info("worker started", "provider", providerURL, "health", health.Addr,
		"rate_per_channel", cfg.RatePerChannel, "concurrency", cfg.Concurrency)

	w.Run(ctx)
	slog.Info("worker stopped")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return health.Shutdown(shutdownCtx)
}
