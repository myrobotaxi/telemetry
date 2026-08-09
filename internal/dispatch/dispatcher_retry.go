package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// commandNavigationRequest is the Tesla command name for a share-to-nav push.
// UNSIGNED — Tesla processes it server-side, so the tesla-http-proxy returns
// ErrCommandUseRESTAPI and forwards it to the Fleet REST API (vehicle-command
// pkg/proxy command.go:545, proxy.go:325). Dispatch uses THIS command, not
// navigation_gps_request: the official proxy's ExtractCommandAction has NO
// case for navigation_gps_request, so it hits the default branch and 400s
// `invalid_command` locally, before the car is ever dialed — the Jul 18 2026
// dispatch_error invalid_request outage (MYR-245).
const commandNavigationRequest = "navigation_request"

// coordShareValue builds the text destination for the navigation_request
// share (it lands in android.intent.extra.TEXT). Used for BOTH legs — the
// pickup on accept and the dropoff on board (MYR-265). The value is text the
// CAR's own nav geocoder resolves — NOT a machine coordinate field. A raw
// "<lat>,<lon>" coordinate pair (full precision, strconv 'f', -1 — never
// truncated) is chosen as the LEAST-AMBIGUOUS text, matching how
// Teslemetry/Tessie-class clients pass coordinates: they forward the raw
// caller text verbatim, not a synthesized maps URL (no authoritative evidence
// the car reliably resolves a maps-URL share into a nav destination, and a
// failure would be silent — the Fleet REST share returns success even if the
// car cannot parse it). On-car acceptance of this shape is settled by the
// MYR-245 live verification; if it fails on-car the fallbacks are the place
// address label or a maps URL.
func coordShareValue(lat, lon float64) string {
	return strconv.FormatFloat(lat, 'f', -1, 64) + "," + strconv.FormatFloat(lon, 'f', -1, 64)
}

// validCoord reports whether a leg coordinate (pickup or dropoff) is within the
// valid WGS-84 range. An out-of-range coordinate is a permanent bad input: we
// refuse it before building the share value, so no malformed destination is
// ever dialed to the car.
func validCoord(p events.RidePlace) bool {
	return p.Latitude >= -90 && p.Latitude <= 90 &&
		p.Longitude >= -180 && p.Longitude <= 180
}

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
	// codeNavSuperseded marks a leg that was NOT pushed (or was stopped
	// mid-push) because a LATER leg of the same ride had already taken the
	// car's navigation (MYR-526). Recorded with outcome `skipped`, not
	// `failed`: nothing went wrong — the ride simply moved on, and pushing the
	// older target would have dragged the dash backwards. It is a distinct code
	// so this is legible in the row and the audit line instead of masquerading
	// as a cancellation.
	codeNavSuperseded = "nav_superseded"
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

// executeWithRetry runs the navigation_request share for a leg's coordinate
// (pickup or dropoff, MYR-265) under the bounded retry policy. Retryable codes
// (transport / asleep-after-wake) back off and retry up to MaxRetries; every
// other code is terminal. It also returns the opaque Tesla-side detail
// (CommandError.Detail, e.g. `invalid_command`) so the outcome log can explain
// WHY a command was rejected.
func (d *Dispatcher) executeWithRetry(ctx context.Context, vin, token string, place events.RidePlace) (outcome Outcome, errCode *string, detail string) {
	// Guard the coordinate before building the share value: an out-of-range
	// coordinate is a permanent bad input, so fail terminally with the typed
	// invalid_request code WITHOUT dialing Tesla.
	if !validCoord(place) {
		code := string(wserrors.ErrCodeInvalidRequest)
		return OutcomeFailed, &code, "coordinate_out_of_range"
	}

	req := commands.Request{
		VIN:     vin,
		Command: commandNavigationRequest,
		Params: map[string]any{
			"value": coordShareValue(place.Latitude, place.Longitude),
		},
		AccessToken: token,
		Scopes:      commands.ParseScopes(token),
	}

	attempts := d.cfg.MaxRetries + 1
	lastCode := string(wserrors.ErrCodeCommandFailed)
	var lastDetail string
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoffFor(d.cfg.Backoff, attempt)); err != nil {
				c := codeCanceled
				return OutcomeFailed, &c, ""
			}
		}

		_, err := d.executor.Execute(ctx, req)
		if err == nil {
			return OutcomeSent, nil, ""
		}
		var retry bool
		retry, lastCode, lastDetail = classifyCommandErr(err)
		if !retry {
			return OutcomeFailed, &lastCode, lastDetail
		}
	}
	// Retries exhausted on a retryable error.
	return OutcomeFailed, &lastCode, lastDetail
}

// classifyCommandErr decides whether a command error is worth another bounded
// attempt and returns the opaque code to record. Retryability is the
// executor's own signal (CommandError.Retryable) — a transient transport /
// vehicle failure or asleep-after-wake. The unconfigured-transport sentinel is
// a permanent misconfiguration: never retry it, and record a distinct code so
// ops can tell it apart from a live command_failed 502. A non-typed error is
// treated as a transient transport failure.
func classifyCommandErr(err error) (retryable bool, code, detail string) {
	if errors.Is(err, commands.ErrTransportNotConfigured) {
		return false, codeTransportUnconfigured, ""
	}
	var cmdErr *commands.CommandError
	if errors.As(err, &cmdErr) {
		return cmdErr.Retryable, string(cmdErr.Code), cmdErr.Detail
	}
	return true, string(wserrors.ErrCodeCommandFailed), ""
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
