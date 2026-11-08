package llm

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

type Config struct {
	Provider string // "claude" or "mock"
	APIKey   string
	Model    string // "" = DefaultModel
	// Per-step effort overrides; "" keeps the default.
	EffortAudience string
	EffortMessages string
	// RefusalFallback "false" disables server-side fallback on refusals.
	RefusalFallback string
}

func ConfigFromEnv() Config {
	return Config{
		Provider:        os.Getenv("LLM_PROVIDER"),
		APIKey:          os.Getenv("ANTHROPIC_API_KEY"),
		Model:           os.Getenv("ANTHROPIC_MODEL"),
		EffortAudience:  os.Getenv("LLM_EFFORT_AUDIENCE"),
		EffortMessages:  os.Getenv("LLM_EFFORT_MESSAGES"),
		RefusalFallback: os.Getenv("LLM_REFUSAL_FALLBACK"),
	}
}

var efforts = []string{"low", "medium", "high", "xhigh", "max"}

// New picks the implementation: the mock when LLM_PROVIDER=mock or no API key is set
// (so `docker compose up` works without a key), Claude otherwise. The returned name is
// for logging which one is active.
func New(cfg Config) (client Client, name string, err error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch {
	case provider == "mock", provider == "" && cfg.APIKey == "", provider == "claude" && cfg.APIKey == "":
		// Slow enough streaming that the UI visibly fills in, like a real model.
		m := NewMock()
		m.ChunkRunes, m.ChunkDelay = 6, 25*time.Millisecond
		return m, "mock", nil
	case provider == "claude", provider == "":
		cc, err := claudeConfig(cfg)
		if err != nil {
			return nil, "", err
		}
		return NewClaude(cc), "claude:" + cc.Model, nil
	default:
		return nil, "", fmt.Errorf("llm: unknown LLM_PROVIDER %q (want claude or mock)", cfg.Provider)
	}
}

func claudeConfig(cfg Config) (ClaudeConfig, error) {
	cc := DefaultClaudeConfig()
	cc.APIKey = cfg.APIKey
	if m := strings.TrimSpace(cfg.Model); m != "" {
		cc.Model = m
	}
	for step, v := range map[Step]string{StepAudience: cfg.EffortAudience, StepMessages: cfg.EffortMessages} {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if !slices.Contains(efforts, v) {
			return ClaudeConfig{}, fmt.Errorf("llm: invalid effort %q for %s (want one of %s)", v, step, strings.Join(efforts, ", "))
		}
		cc.Effort[step] = v
	}
	if strings.EqualFold(strings.TrimSpace(cfg.RefusalFallback), "false") {
		cc.RefusalFallback = false
	}
	return cc, nil
}
