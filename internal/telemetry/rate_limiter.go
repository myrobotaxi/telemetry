package telemetry

import (
	"sync"
	"time"
)

// rateLimiter implements per-VIN token-bucket rate limiting. Each vehicle
// gets its own bucket with a configurable maximum rate. The limiter is
// safe for concurrent use.
type rateLimiter struct {
	maxPerSec float64
	mu        sync.Mutex
	buckets   map[string]*bucket
}

// bucket tracks rate-limit state for a single vehicle.
type bucket struct {
	tokens   float64
	lastFill time.Time
}

// newRateLimiter creates a rate limiter that allows maxPerSec messages
// per second per vehicle. If maxPerSec is zero or negative, the limiter
// allows all messages.
func newRateLimiter(maxPerSec float64) *rateLimiter {
	return &rateLimiter{
		maxPerSec: maxPerSec,
		buckets:   make(map[string]*bucket),
	}
}

// allow reports whether a message from the given VIN should be processed.
// It returns false when the vehicle has exceeded its rate limit.
func (rl *rateLimiter) allow(vin string) bool {
	if rl.maxPerSec <= 0 {
		return true
	}

	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[vin]
	if !ok {
		b = &bucket{tokens: rl.maxPerSec, lastFill: now}
		rl.buckets[vin] = b
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * rl.maxPerSec
	if b.tokens > rl.maxPerSec {
		b.tokens = rl.maxPerSec
	}
	b.lastFill = now

	if b.tokens < 1 {
		return false
	}

	b.tokens--
	return true
}

// remove deletes the rate-limit state for a vehicle. Call this when a
// vehicle disconnects to avoid leaking memory.
func (rl *rateLimiter) remove(vin string) {
	rl.mu.Lock()
	delete(rl.buckets, vin)
	rl.mu.Unlock()
}
