package backoff

import (
	"testing"
	"time"
)

func TestEqualJitterRange(t *testing.T) {
	for range 1000 {
		if d := EqualJitter(4 * time.Second); d < 2*time.Second || d > 4*time.Second {
			t.Fatalf("jitter %v outside [2s, 4s]", d)
		}
	}
	if EqualJitter(0) != 0 {
		t.Fatal("zero backoff must stay zero")
	}
}

func TestExponential(t *testing.T) {
	cases := []struct {
		attempt int
		max     time.Duration
		want    time.Duration
	}{
		{1, 0, time.Second},
		{2, 0, 2 * time.Second},
		{5, 0, 16 * time.Second},
		{6, 30 * time.Second, 30 * time.Second},
		{0, 0, time.Second},
	}
	for _, c := range cases {
		if got := Exponential(time.Second, c.attempt, c.max); got != c.want {
			t.Errorf("Exponential(1s, %d, %v) = %v, want %v", c.attempt, c.max, got, c.want)
		}
	}
}
