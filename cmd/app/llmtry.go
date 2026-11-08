package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denizsahiner/smart-campaign-composer/internal/compose"
	"github.com/denizsahiner/smart-campaign-composer/internal/llm"
	"github.com/denizsahiner/smart-campaign-composer/internal/prompt"
	"github.com/denizsahiner/smart-campaign-composer/internal/seed"
	"github.com/denizsahiner/smart-campaign-composer/internal/segment"
)

// runLLMTry runs both LLM steps for one brief and prints what happened: a developer tool
// for trying prompts and models against the real API. It uses the database for cities,
// reach and the longest name when DATABASE_URL is set, and the seed's city list otherwise.
func runLLMTry(ctx context.Context, args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return errors.New(`usage: app llm-try "<brief>"`)
	}
	brief := args[0]

	client, name, err := llm.New(llm.ConfigFromEnv())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "provider: %s\n", name)

	cities, longest := seed.Cities(), 15
	var pool *pgxpool.Pool
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		if pool, err = pgxpool.New(ctx, dsn); err != nil {
			return err
		}
		defer pool.Close()
		if cities, err = segment.KnownCities(ctx, pool); err != nil {
			return err
		}
	}

	rec := &printRecorder{}
	c := compose.New(client, rec, compose.DefaultPolicy())

	fmt.Fprintln(os.Stderr, "\n== audience")
	a, err := c.Audience(ctx, 0, brief, segment.NewValidator(cities))
	printJSON("unsupported_criteria", a.Unsupported)
	if err != nil {
		return fmt.Errorf("audience: %w", err)
	}
	printJSON("segment", a.Segment.Describe())
	if pool != nil {
		counts, err := segment.Count(ctx, pool, a.Segment, time.Now())
		if err != nil {
			return err
		}
		printJSON("reach", counts)
		longest = counts.LongestFirstName
	}

	fmt.Fprintln(os.Stderr, "\n== messages (streaming)")
	res, err := c.Messages(ctx, 0, prompt.MessagesInput{Brief: brief, Segment: a.Segment, LongestFirstName: longest},
		func(e compose.Event) {
			switch e.Type {
			case compose.EventDelta:
				fmt.Fprint(os.Stderr, e.Delta)
			case compose.EventRetry:
				fmt.Fprintf(os.Stderr, "\n-- retry (%s)\n", e.Reason)
			default:
				fmt.Fprintf(os.Stderr, "\n-- %s\n", e.Type)
			}
		})
	if err != nil {
		return fmt.Errorf("messages: %w", err)
	}
	printJSON("content", res.Content)
	printJSON("valid", res.Valid)
	if !res.Valid {
		printJSON("errors", res.Errors)
	}

	in, out := 0, 0
	for _, call := range rec.calls {
		in, out = in+call.InputTokens, out+call.OutputTokens
	}
	fmt.Fprintf(os.Stderr, "\ncalls: %d, input tokens: %d, output tokens: %d\n", len(rec.calls), in, out)
	return nil
}

type printRecorder struct{ calls []compose.Call }

func (r *printRecorder) RecordCall(_ context.Context, c compose.Call) error {
	r.calls = append(r.calls, c)
	status := "ok"
	switch {
	case c.Error != "":
		status = "error: " + c.Error
	case c.ValidationErrors != nil:
		b, _ := json.Marshal(c.ValidationErrors)
		status = "invalid: " + string(b)
	}
	fmt.Fprintf(os.Stderr, "\n[call] step=%s repair=%d transport=%d in=%d out=%d latency=%s %s\n",
		c.Step, c.RepairAttempt, c.TransportAttempt, c.InputTokens, c.OutputTokens, c.Latency.Round(time.Millisecond), status)
	return nil
}

func printJSON(label string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Printf("%s: %s\n", label, b)
}
