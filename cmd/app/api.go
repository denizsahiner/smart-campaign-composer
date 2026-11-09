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

	"github.com/denizsahiner/smart-campaign-composer/internal/api"
	"github.com/denizsahiner/smart-campaign-composer/internal/compose"
	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
	"github.com/denizsahiner/smart-campaign-composer/internal/store"
)

func listenAddr() string { return envString("API_ADDR", ":8080") }

// runAPI serves the HTTP API until ctx is cancelled (SIGTERM), then drains open requests.
func runAPI(ctx context.Context) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// The city list is fixed once the seed is loaded (migrate runs before the API).
	cities, err := segment.KnownCities(ctx, pool)
	if err != nil {
		return err
	}
	client, provider, err := llm.New(llm.ConfigFromEnv())
	if err != nil {
		return err
	}
	composer := compose.New(client, store.New(pool), compose.DefaultPolicy())
	srv := &http.Server{
		Addr:              listenAddr(),
		Handler:           api.NewServer(pool, composer, segment.NewValidator(cities)).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: message generation is a long-lived SSE response.
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("api listening", "addr", srv.Addr, "llm", provider, "cities", len(cities))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("api shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// runHealthcheck is the container healthcheck: the runtime image has no curl. Without an
// argument it checks the API; the worker and the mock provider pass their own URL.
func runHealthcheck(args []string) error {
	url := "http://localhost" + listenAddr() + "/healthz"
	if len(args) > 0 {
		url = args[0]
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}
