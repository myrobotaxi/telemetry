package telemetry

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2" //nolint:gosec // jitter for backoff, not security
	"net/http"
	"strconv"
	"time"
)

const (
	defaultMaxRetries = 3
	defaultBaseDelay  = 1 * time.Second
	defaultMaxDelay   = 30 * time.Second
	retryAfterHeader  = "Retry-After"
)

// retryPolicy governs how the Fleet API client retries failed requests.
type retryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

// defaultRetryPolicy returns a sensible retry configuration.
func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		MaxRetries: defaultMaxRetries,
		BaseDelay:  defaultBaseDelay,
		MaxDelay:   defaultMaxDelay,
	}
}

// isRetryable reports whether the HTTP status code warrants a retry.
// We retry on 429 (rate limited) and 5xx (server errors) — with ONE carve-out.
//
// 503 IS NOT RETRIED (MYR-394). On the Fleet API, 503 is not "our server is
// briefly unwell", it is Tesla's answer for a VEHICLE that is asleep, offline,
// or transitioning between states — the sibling of 408, and this package
// already treats the two as one condition (isVehicleUnreachable in
// ride_position_poller_loop.go). No amount of backoff makes an unavailable
// vehicle available: the car wakes when its driver gets in, not seven seconds
// from now.
//
// The cost of getting this wrong is concentrated on exactly the cars this
// codebase reads most. With MaxRetries=3 and 1s/2s/4s (±25%) backoff, all four
// attempts fit inside the ride poller's 15s CallTimeout, so one asleep car on
// an active ride cost FOUR vehicle_data GETs every 25s cycle — 9.6 GETs/min
// against a car that had already said it was not answering — and every one of
// them was invisible, because the poller logs 503 at Debug and the per-attempt
// warning goes to the Fleet API client's own logger.
//
// 500 / 502 / 504 keep their retries: those really are transient server faults.
func isRetryable(statusCode int) bool {
	if statusCode == http.StatusServiceUnavailable {
		return false
	}
	return statusCode == http.StatusTooManyRequests || statusCode >= 500
}

// retryDelay computes the wait duration for a given retry attempt.
// If the response includes a Retry-After header (from a 429), that
// value takes precedence over exponential backoff.
func retryDelay(resp *http.Response, attempt int, policy retryPolicy) time.Duration {
	// Prefer the server-provided Retry-After value.
	if resp != nil {
		if ra := resp.Header.Get(retryAfterHeader); ra != "" {
			if seconds, err := strconv.Atoi(ra); err == nil && seconds > 0 {
				return time.Duration(seconds) * time.Second
			}
		}
	}

	// Exponential backoff with ±25% jitter to prevent thundering herd.
	delay := time.Duration(float64(policy.BaseDelay) * math.Pow(2, float64(attempt)))
	if delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	// Apply jitter: multiply by [0.75, 1.25).
	jitter := 0.75 + rand.Float64()*0.5 // #nosec G404 -- backoff jitter
	return time.Duration(float64(delay) * jitter)
}

// sleepWithContext blocks for the given duration, returning early if
// the context is cancelled.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("retry wait cancelled: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
