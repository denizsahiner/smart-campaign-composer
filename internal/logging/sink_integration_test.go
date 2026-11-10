package logging

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/denizsahiner/smart-campaign-composer/internal/dbtest"
)

func TestSinkWritesAndPrunes(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	var console bytes.Buffer
	cfg := Config{Service: "api", Level: slog.LevelInfo, Out: &console}
	sink := NewSink(pool, cfg.Console())
	log := New(cfg, sink.Handler(cfg.Level))

	rctx := WithScope(ctx)
	Add(rctx, "request_id", "req-1")
	Add(rctx, "campaign_id", int64(42))
	log.InfoContext(rctx, "request") // stdout only
	log.ErrorContext(rctx, "request failed", "err", "db down", "path", "/api/campaigns/42")
	log.Warn("channel throttled by provider", "channel", "sms", "pause", 2*time.Second)
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sink.Close(closeCtx)

	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM app_logs`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d rows, want 2 (info stays on stdout)", n)
	}
	var level, service, msg, reqID, path string
	var campaignID int64
	if err := pool.QueryRow(ctx, `SELECT level, service, msg, request_id, campaign_id, attrs->>'path'
		FROM app_logs WHERE level = 'error'`).Scan(&level, &service, &msg, &reqID, &campaignID, &path); err != nil {
		t.Fatal(err)
	}
	if service != "api" || msg != "request failed" || reqID != "req-1" || campaignID != 42 || path != "/api/campaigns/42" {
		t.Errorf("row = %s %s %s %s %d %s", level, service, msg, reqID, campaignID, path)
	}
	var pause string
	pool.QueryRow(ctx, `SELECT attrs->>'pause' FROM app_logs WHERE level = 'warn' AND campaign_id IS NULL`).Scan(&pause)
	if pause != "2s" {
		t.Errorf("pause = %q, want 2s", pause)
	}
	if !bytes.Contains(console.Bytes(), []byte(`"msg":"request"`)) {
		t.Error("the info line didn't reach stdout")
	}

	if _, err := pool.Exec(ctx, `UPDATE app_logs SET ts = now() - interval '8 days' WHERE level = 'warn'`); err != nil {
		t.Fatal(err)
	}
	pruned, err := Prune(ctx, pool, 7*24*time.Hour)
	if err != nil || pruned != 1 {
		t.Fatalf("pruned %d, err %v; want 1", pruned, err)
	}
}
