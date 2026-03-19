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
	defaultMaxRetries   = 3
	defaultBaseDelay    = 1 * time.Second
	defaultMaxDelay     = 30 * time.Second
	retryAfterHeader    = "Retry-After"
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
// We retry on 429 (rate limited) and 5xx (server errors).
func isRetryable(statusCode int) bool {
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
