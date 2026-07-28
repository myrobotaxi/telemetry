package commands

import (
	"context"
	"log/slog"
)

// AwakeProbe reports whether the vehicle is awake enough to serve the caller's
// next Fleet API call. It is the seam that lets EnsureAwake bound a wake
// WITHOUT knowing what the caller intends to do afterwards: a command driver
// probes by re-issuing the command, while an owner-triggered read (the MYR-315
// refresh endpoint) probes with the cheap GET /api/1/vehicles/{vin} object and
// checks its `state` field.
//
// A probe MUST NOT itself force-wake the car, and MUST be cheap enough to run
// once per wake attempt (the loop calls it up to WakeMaxAttempts+1 times).
// An error is terminal: EnsureAwake surfaces it rather than burning the budget
// against a probe that is failing for reasons a wake cannot fix.
type AwakeProbe func(ctx context.Context) (awake bool, err error)

// EnsureAwake drives a vehicle to an awake state under the SAME bounded
// wake+retry policy Execute applies to commands — the same WakeMaxAttempts
// budget, the same WakeBackoff spacing, the same transport.Wake call, and the
// same terminal vehicle_asleep (503) typed error once the budget is spent.
// It exists so callers outside this package reuse that machinery instead of
// forking a second, silently divergent wake loop (MYR-315).
//
// The probe runs FIRST, so an already-awake vehicle costs exactly one probe and
// zero wake calls — the common case for the refresh endpoint, where a stale
// stream usually means "online but not pushing frames", not "asleep".
//
// The loop always makes progress: every iteration either returns or consumes
// one unit of the wake budget, so it cannot spin.
func (e *Executor) EnsureAwake(ctx context.Context, vin, token string, probe AwakeProbe) error {
	attempts := 0
	for {
		awake, err := probe(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return errInternal("canceled during wake probe")
			}
			return errCommandFailed("vehicle wake probe failed")
		}
		if awake {
			return nil
		}

		// Checked lazily rather than up front: an already-awake vehicle needs no
		// wake path at all, so a deployment without one must still serve it. Only
		// a car that actually needs waking is failed fast here — spending the
		// budget on a transport that cannot deliver a single wake is pure latency.
		if !e.transport.Enabled() && !e.transport.RESTEnabled() {
			return errTransportNotConfigured()
		}

		if cErr := e.wakeStep(ctx, vin, token, "ensure_awake", &attempts, "vehicle did not wake within the retry budget"); cErr != nil {
			return cErr
		}
	}
}

// wakeStep performs ONE step of the shared wake policy: spend budget, ask the
// transport to wake the car, then wait out the backoff. It returns a non-nil
// *CommandError only when the caller must STOP — either the budget is spent
// (vehicle_asleep, carrying reason as the opaque detail) or the context died
// mid-backoff. A nil return means "budget consumed, try again".
//
// The wake call itself is best-effort by design: Tesla's wake_up is
// fire-and-forget and frequently errors for a car that is already on its way
// up, so a failed wake logs and still burns the backoff rather than aborting.
//
// Shared by Execute's OutcomeAsleep branch and EnsureAwake so the two can never
// drift apart; op names the caller for the log line only.
func (e *Executor) wakeStep(ctx context.Context, vin, token, op string, attempts *int, reason string) *CommandError {
	if *attempts >= e.cfg.WakeMaxAttempts {
		return withDetail(errVehicleAsleep(), reason)
	}
	*attempts++
	if wErr := e.transport.Wake(ctx, vin, token); wErr != nil {
		e.logger.Warn("wake request failed; continuing with the bounded retry budget",
			slog.Int("attempt", *attempts),
			slog.String("op", op),
		)
	}
	if sErr := sleepCtx(ctx, e.cfg.WakeBackoff); sErr != nil {
		return errInternal("canceled during wake backoff")
	}
	return nil
}
