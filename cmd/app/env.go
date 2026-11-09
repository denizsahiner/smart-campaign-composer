package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// envError collects malformed variables, so a typo fails startup instead of silently
// falling back to a default.
type envError []string

func (e *envError) add(name, val string, err error) {
	*e = append(*e, fmt.Sprintf("%s=%q: %v", name, val, err))
}

func (e envError) err() error {
	if len(e) == 0 {
		return nil
	}
	return fmt.Errorf("invalid environment: %v", []string(e))
}

func envString(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func (e *envError) int(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		e.add(name, v, fmt.Errorf("want a non-negative integer"))
		return def
	}
	return n
}

func (e *envError) float(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		e.add(name, v, fmt.Errorf("want a non-negative number"))
		return def
	}
	return f
}

func (e *envError) duration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		e.add(name, v, fmt.Errorf("want a duration like 200ms or 30s"))
		return def
	}
	return d
}

func (e *envError) uint64(name string) uint64 {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		e.add(name, v, err)
	}
	return n
}
