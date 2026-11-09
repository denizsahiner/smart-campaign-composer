package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/denizsahiner/smart-campaign-composer/internal/provider"
)

func providerConfigFromEnv() (provider.Config, error) {
	var e envError
	d := provider.DefaultConfig()
	cfg := provider.Config{
		RatePerChannel: e.float("PROVIDER_RATE_PER_CHANNEL", d.RatePerChannel),
		ErrorRate:      e.float("PROVIDER_ERROR_RATE", d.ErrorRate),
		TimeoutRate:    e.float("PROVIDER_TIMEOUT_RATE", d.TimeoutRate),
		ThrottleEvery:  e.duration("PROVIDER_THROTTLE_EVERY", d.ThrottleEvery),
		ThrottleFor:    e.duration("PROVIDER_THROTTLE_FOR", d.ThrottleFor),
		LatencyMin:     time.Duration(e.int("PROVIDER_LATENCY_MIN_MS", int(d.LatencyMin.Milliseconds()))) * time.Millisecond,
		LatencyMax:     time.Duration(e.int("PROVIDER_LATENCY_MAX_MS", int(d.LatencyMax.Milliseconds()))) * time.Millisecond,
		Seed:           e.uint64("PROVIDER_SEED"),
	}
	return cfg, e.err()
}

// runProvider serves the mock SMS/email provider until ctx is cancelled.
func runProvider(ctx context.Context) error {
	cfg, err := providerConfigFromEnv()
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              envString("PROVIDER_ADDR", ":8090"),
		Handler:           provider.New(cfg).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("mock provider listening", "addr", srv.Addr, "rate_per_channel", cfg.RatePerChannel,
		"error_rate", cfg.ErrorRate, "timeout_rate", cfg.TimeoutRate,
		"throttle_every", cfg.ThrottleEvery, "throttle_for", cfg.ThrottleFor)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Injected hangs end when their client disconnects; don't wait for them.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return srv.Close()
	}
	return nil
}
