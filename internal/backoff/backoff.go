// Package backoff holds the retry wait shared by Claude calls and message sends.
package backoff

import (
	"math/rand/v2"
	"time"
)

// EqualJitter returns a wait between d/2 and d: at least half the computed backoff, so a
// retry never fires almost immediately, with the other half random so a herd of retries
// spreads out.
func EqualJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}

// Exponential is base doubled per attempt (1-based): base, 2×base, 4×base, ... capped at max
// when max > 0.
func Exponential(base time.Duration, attempt int, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if max > 0 && d >= max {
			return max
		}
	}
	if max > 0 && d > max {
		return max
	}
	return d
}
