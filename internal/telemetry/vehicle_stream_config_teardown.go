package telemetry

import (
	"context"
	"log/slog"
)

// StreamConfigTeardown is THE one implementation of "stop this VIN streaming at
// Tesla" (car-offboarding.md §5.2). Both severing paths hold one: the
// per-vehicle teardown endpoint (one car) and the account-deletion sequence
// (every car the account owns, MYR-593).
//
// WHY IT IS A TYPE AND NOT A METHOD ON EACH HANDLER. It used to be a private
// method on VehicleTeardownHandler, and the account-deletion sequence — which
// calls the SAME store.OwnerTeardown transaction one car at a time — did not
// have it. So "remove a vehicle" meant two different things depending on which
// door you came through: the endpoint deleted the Tesla-side config, and the
// account deletion silently did not. The consequence was a pure cost leak. A
// deleted account's cars kept streaming and kept billing until the fleet
// config's 350-day `exp` lapsed, and nothing could ever reach them again: the
// MYR-592 inactivity sweeper joins from a live `Vehicle` row, and the account
// deletion had just removed it. Extracting the step is what makes the two paths
// incapable of drifting apart again.
//
// EVERY FAILURE IS A SKIP. An unwired deleter (no tesla-http-proxy configured,
// which is every test and every dev process), an empty VIN, no token on file, a
// Tesla 404 for a config that was never applied, a Tesla 5xx — all of them
// return false and log. None of them is an error, because the authoritative
// teardown is the local database transaction and a person's deletion of their
// own account must not be blockable by Tesla's availability (MYR-366).
type StreamConfigTeardown struct {
	fleet  FleetConfigDeleter
	tokens teslaTokenResolver
	logger *slog.Logger
}

// NewStreamConfigTeardown wires the step. EITHER dependency may be nil and the
// result is still usable — DeleteStreamConfig then reports false without
// calling anything, which is the ordinary state of a deployment with no proxy
// configured. Returning a working value rather than nil is deliberate: a nil
// *StreamConfigTeardown would push the not-configured check back out to every
// caller, which is the shape this type exists to remove.
func NewStreamConfigTeardown(
	fleet FleetConfigDeleter,
	tokens teslaTokenResolver,
	logger *slog.Logger,
) *StreamConfigTeardown {
	if logger == nil {
		logger = slog.Default()
	}
	return &StreamConfigTeardown{fleet: fleet, tokens: tokens, logger: logger}
}

// DeleteStreamConfig best-effort deletes one vehicle's Tesla telemetry config
// and reports whether Tesla accepted it. It NEVER returns an error: see the
// type doc for why a false is always a skip and never a stop.
//
// userID names whose token authenticates the call, which for both callers is
// the car's owner. It MUST be invoked while that token still exists — before
// the OAuth grant is revoked at Tesla and before the "Account" row holding the
// tokens is deleted. Both callers order themselves around that.
func (t *StreamConfigTeardown) DeleteStreamConfig(ctx context.Context, userID, vin string) bool {
	if t == nil || t.fleet == nil || t.tokens == nil || vin == "" {
		return false
	}

	tok, err := t.tokens.Resolve(ctx, userID)
	if err != nil {
		// Expected on the re-run path and for any owner whose link has already
		// lapsed. Warn rather than Error: there is nothing an operator can do,
		// and the config self-expires at Tesla.
		t.logger.Warn("stream-config delete skipped: no Tesla token",
			slog.String("user_id", userID),
			slog.String("vin", redactVIN(vin)),
		)
		return false
	}

	if err := t.fleet.DeleteTelemetryConfig(ctx, tok.AccessToken, vin); err != nil {
		// Non-fatal. The error text comes from FleetAPIClient, which already
		// redacts the VIN and never echoes the token.
		t.logger.Warn("stream-config delete failed (non-fatal)",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		return false
	}

	// Audit line. P0 only: an opaque user cuid and a last-4 VIN, never the
	// token (data-classification.md §2.1).
	t.logger.Info("tesla_stream_config_deleted",
		slog.String("event", "tesla_stream_config_deleted"),
		slog.String("user_id", userID),
		slog.String("vin", redactVIN(vin)),
	)
	return true
}
