package llm

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Provider string // "claude" or "mock"
	APIKey   string
}

func ConfigFromEnv() Config {
	return Config{
		Provider: os.Getenv("LLM_PROVIDER"),
		APIKey:   os.Getenv("ANTHROPIC_API_KEY"),
	}
}

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
		return nil, "", errors.New("llm: claude client not implemented yet; set LLM_PROVIDER=mock")
	default:
		return nil, "", fmt.Errorf("llm: unknown LLM_PROVIDER %q (want claude or mock)", cfg.Provider)
	}
}
