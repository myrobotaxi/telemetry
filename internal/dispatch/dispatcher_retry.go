package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// commandNavigationGPS is the Tesla command name for a lat/long navigation
// push (MYR-180 registry). UNSIGNED — forwarded straight to the Fleet API.
const commandNavigationGPS = "navigation_gps_request"

// pickupNavOrder is the Tesla remote-nav "order" integer for the pickup
// destination. Tesla's convention is 1-based: order=1 REPLACES the current
// trip (making the pickup THE active destination), 2 prepends a stop, 3
// appends a stop. We want the pickup to be the immediate destination, so we
// send 1 — matching internal/commands buildNavigationGPS's default. (Source:
// Tesla Fleet API navigation_gps_request remote-nav order semantics, as
// documented by the Teslemetry python-tesla-fleet-api client:
// https://github.com/Teslemetry/python-tesla-fleet-api — "1 replaces the
// trip, 2 prepends a stop, 3 appends a stop".)
const pickupNavOrder = float64(1)

// Non-command failure codes recorded in dispatch_error when the pipeline
// fails BEFORE (or instead of) a Tesla command. Command failures record the
// typed wserrors code (key_not_paired, vehicle_asleep, …) verbatim.
//
// The standalone `// #nosec G101` lines below suppress gosec's hardcoded-
// credential false positive: these are OPAQUE outcome codes surfaced in
// logs + the P0 dispatch_error column, never credentials. gosec matches the
// nosec directive on the line directly above the flagged const spec, so the
// comment MUST stay standalone (a trailing `//nolint:gosec // #nosec` on the
// same line is NOT honored by the standalone gosec scanner).
const (
	codeVehicleUnresolved = "vehicle_unresolved"
	// #nosec G101 -- opaque outcome code, not a credential
	codeTokenUnavailable = "token_unavailable"
	// #nosec G101 -- opaque outcome code, not a credential
	codeTokenExpired          = "token_expired"
	codeTransportUnconfigured = "transport_unconfigured"
	codeCanceled              = "dispatch_canceled"
	// codeDispatchInterrupted marks a dispatch claimed (dispatched_at set) but
	// whose process died before it recorded an outcome — a crash/SIGTERM in
	// the claim→record window. Set by the startup reconciler.
	codeDispatchInterrupted = "dispatch_interrupted"
)

// Permanent (non-retryable) resolution-failure sentinels. The cmd/ adapters
// map the concrete telemetry/store sentinels (ErrTeslaTokenExpired,
// ErrTeslaTokenUnavailable, store.ErrVehicleNotFound) onto these so the
// dispatcher can classify a resolution failure as permanent WITHOUT importing
// those packages (keeping the consumer-site interface boundary clean). Any
// OTHER resolution error is treated as transient and retried under the
// bounded policy.
var (
	// ErrTokenExpired — a token exists but is expired and cannot be
	// refreshed; the owner must re-link. Recorded as token_expired.
	ErrTokenExpired = errors.New("dispatch: tesla token expired")
	// ErrTokenUnavailable — no token on file (account never linked).
	// Recorded as token_unavailable.
	ErrTokenUnavailable = errors.New("dispatch: tesla token unavailable")
	// ErrVehicleNotFound — the vehicle row is gone; no VIN to resolve.
	// Recorded as vehicle_unresolved.
	ErrVehicleNotFound = errors.New("dispatch: vehicle not found")
)

// resolveWithRetry runs fn under the bounded retry policy (MaxRetries extra
// attempts with exponential backoff). It retries only transient errors:
// a permanentResolution error, or a dead ctx, stops the loop immediately.
func (d *Dispatcher) resolveWithRetry(ctx context.Context, fn func(context.Context) (string, error)) (string, error) {
	attempts := d.cfg.MaxRetries + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoffFor(d.cfg.Backoff, attempt)); err != nil {
				return "", err
			}
		}
		v, err := fn(ctx)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if permanentResolution(err) || ctx.Err() != nil {
			return "", err
		}
	}
	return "", lastErr
}

// permanentResolution reports whether a resolution error is a well-identified
// permanent condition that no retry can fix.
func permanentResolution(err error) bool {
	return errors.Is(err, ErrTokenExpired) ||
		errors.Is(err, ErrTokenUnavailable) ||
		errors.Is(err, ErrVehicleNotFound)
}

// tokenErrorCode maps a failed token resolution to its opaque outcome code.
func tokenErrorCode(err error) string {
	switch {
	case isContextErr(err):
		return codeCanceled
	case errors.Is(err, ErrTokenExpired):
		return codeTokenExpired
	case errors.Is(err, ErrTokenUnavailable):
		return codeTokenUnavailable
	default:
		// Transient failure that exhausted the retry budget: the token could
		// not be obtained, so it is effectively unavailable.
		return codeTokenUnavailable
	}
}

// isContextErr reports whether err is (or wraps) a context cancellation or
// deadline — used to record dispatch_canceled rather than a resolution code.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// executeWithRetry runs the navigation_gps_request under the bounded retry
// policy. Retryable codes (transport / asleep-after-wake) back off and retry
// up to MaxRetries; every other code is terminal.
func (d *Dispatcher) executeWithRetry(ctx context.Context, vin, token string, pickup events.RidePlace) (outcome Outcome, errCode *string) {
	req := commands.Request{
		VIN:     vin,
		Command: commandNavigationGPS,
		Params: map[string]any{
			"lat":   pickup.Latitude,
			"lon":   pickup.Longitude,
			"order": pickupNavOrder,
		},
		AccessToken: token,
		Scopes:      commands.ParseScopes(token),
	}

	attempts := d.cfg.MaxRetries + 1
	lastCode := string(wserrors.ErrCodeCommandFailed)
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoffFor(d.cfg.Backoff, attempt)); err != nil {
				c := codeCanceled
				return OutcomeFailed, &c
			}
		}

		_, err := d.executor.Execute(ctx, req)
		if err == nil {
			return OutcomeSent, nil
		}
		var retry bool
		retry, lastCode = classifyCommandErr(err)
		if !retry {
			return OutcomeFailed, &lastCode
		}
	}
	// Retries exhausted on a retryable error.
	return OutcomeFailed, &lastCode
}

// classifyCommandErr decides whether a command error is worth another bounded
// attempt and returns the opaque code to record. Retryability is the
// executor's own signal (CommandError.Retryable) — a transient transport /
// vehicle failure or asleep-after-wake. The unconfigured-transport sentinel is
// a permanent misconfiguration: never retry it, and record a distinct code so
// ops can tell it apart from a live command_failed 502. A non-typed error is
// treated as a transient transport failure.
func classifyCommandErr(err error) (retryable bool, code string) {
	if errors.Is(err, commands.ErrTransportNotConfigured) {
		return false, codeTransportUnconfigured
	}
	var cmdErr *commands.CommandError
	if errors.As(err, &cmdErr) {
		return cmdErr.Retryable, string(cmdErr.Code)
	}
	return true, string(wserrors.ErrCodeCommandFailed)
}

// backoffFor grows the base backoff exponentially per retry attempt
// (attempt is 1-based for the first retry): base, 2·base, 4·base, …
func backoffFor(base time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return base
	}
	return base << (attempt - 1)
}

// sleepCtx sleeps for d or returns early if ctx is canceled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("dispatch backoff: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
