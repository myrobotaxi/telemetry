package telemetry

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsWithinLimit(t *testing.T) {
	t.Parallel()

	rl := newRateLimiter(5) // 5 per second

	for i := range 5 {
		if !rl.allow("VIN1") {
			t.Errorf("request %d should be allowed within rate limit", i)
		}
	}
}

func TestRateLimiter_BlocksOverLimit(t *testing.T) {
	t.Parallel()

	rl := newRateLimiter(3)

	allowed := 0
	for range 10 {
		if rl.allow("VIN1") {
			allowed++
		}
	}

	if allowed > 3 {
		t.Errorf("allowed %d requests, want at most 3", allowed)
	}
	if allowed == 0 {
		t.Error("expected at least 1 request to be allowed")
	}
}

func TestRateLimiter_RefillsOverTime(t *testing.T) {
	t.Parallel()

	rl := newRateLimiter(2)

	// Exhaust tokens.
	rl.allow("VIN1")
	rl.allow("VIN1")

	if rl.allow("VIN1") {
		t.Error("should be blocked after exhausting tokens")
	}

	// Poll until refill occurs (token bucket refills based on elapsed time).
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	refilled := false
	for !refilled {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for rate limiter refill")
		case <-tick.C:
			if rl.allow("VIN1") {
				refilled = true
			}
		}
	}
}

func TestRateLimiter_IndependentPerVIN(t *testing.T) {
	t.Parallel()

	rl := newRateLimiter(2)

	// Exhaust VIN1's tokens.
	rl.allow("VIN1")
	rl.allow("VIN1")

	// VIN2 should still have tokens.
	if !rl.allow("VIN2") {
		t.Error("VIN2 should be allowed independently of VIN1")
	}
}

func TestRateLimiter_ZeroRateAllowsAll(t *testing.T) {
	t.Parallel()

	rl := newRateLimiter(0)

	for i := range 100 {
		if !rl.allow("VIN1") {
			t.Errorf("request %d should be allowed when rate is 0 (unlimited)", i)
		}
	}
}

func TestRateLimiter_NegativeRateAllowsAll(t *testing.T) {
	t.Parallel()

	rl := newRateLimiter(-1)

	for i := range 100 {
		if !rl.allow("VIN1") {
			t.Errorf("request %d should be allowed when rate is negative (unlimited)", i)
		}
	}
}

func TestRateLimiter_Remove(t *testing.T) {
	t.Parallel()

	rl := newRateLimiter(2)

	rl.allow("VIN1")
	rl.allow("VIN1")

	// VIN1 is exhausted.
	if rl.allow("VIN1") {
		t.Error("VIN1 should be blocked")
	}

	// Remove and re-add — should get fresh tokens.
	rl.remove("VIN1")
	if !rl.allow("VIN1") {
		t.Error("VIN1 should be allowed after remove (fresh bucket)")
	}
}
