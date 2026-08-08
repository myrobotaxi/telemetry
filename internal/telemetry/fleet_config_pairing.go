// In-band virtual-key pairing detection for the MYR-489 zero-ops self-heal.
//
// MYR-448's reconciler is blind to state changes: it only knows how many times
// it has failed, so attempts made while the virtual key did not yet exist run
// attempt_count up and bury the car under an hours-deep backoff at exactly the
// moment the key finally pairs. Tester Nabil reached attempt 6 — an 8-hour gap
// — before pairing, and the system was asleep when it mattered.
//
// The missing signal was already arriving. A SIGNED vehicle command that Tesla
// reports as APPLIED cannot succeed without an enrolled virtual key and a
// reachable car, so it is proof of both. internal/commands hands us that event
// (commands.SignedCommandObserver); this file turns it into a backoff reset and
// an immediate per-vehicle reconcile.

package telemetry

import (
	"context"
	"log/slog"
	"time"
)

// pairingSignalBuffer is the depth of the inbox between the command path and
// the reconcile loop.
//
// Sized for a burst, not a backlog. The producer is an owner tapping buttons in
// the app and the consumer is a loop that spends up to CallTimeout per signal,
// so a deep queue would only let stale VINs pile up behind cars whose state has
// since moved on. Overflow is safe by construction: a dropped signal costs
// latency, never correctness, because the periodic pass reaches the same car
// anyway.
const pairingSignalBuffer = 32

// pairingResetDebounce is the minimum gap between two accepted pairing signals
// for the SAME vehicle.
//
// An owner setting up a new car sends commands in bursts; each one is equally
// good proof and none of them changes the answer. Debouncing keeps a ten-tap
// burst to one signed Tesla round-trip, and — more importantly — stops the
// pairing EPOCH timestamp from sliding forward on every tap, since that is the
// clock the once-per-epoch escalation budget is measured against.
const pairingResetDebounce = 10 * time.Minute

// FleetConfigPairingResetter opens a new pairing epoch for a VIN: it clears the
// vehicle's backoff, stamps the epoch, and returns the candidate so the caller
// can reconcile it at once. Satisfied by *store.VehicleRepo via a cmd/ adapter.
//
// found is false (with a nil error) when there is nothing to do — no schedule
// row, unknown VIN, or a debounced repeat.
type FleetConfigPairingResetter interface {
	ResetFleetConfigScheduleOnPairing(
		ctx context.Context, vin string, now, debounceBefore time.Time,
	) (FleetConfigCandidate, bool, error)
}

// SignedCommandApplied implements commands.SignedCommandObserver.
//
// Deliberately a NON-BLOCKING send. It runs on the request goroutine with the
// owner waiting on their command's HTTP response, and the consumer may be
// mid-pass holding a 20-second Tesla call; blocking here would make a healthy
// car's door-unlock hang on an unrelated car's slow config read. A full inbox
// drops the signal and says so — the periodic pass is the backstop.
func (r *FleetConfigReconciler) SignedCommandApplied(vin string) {
	select {
	case r.pairing <- vin:
	default:
		r.logger.Warn("fleet-config reconcile: pairing signal dropped (inbox full)",
			slog.String("event", "fleet_config_pairing_signal_dropped"),
			slog.String("vin", redactVIN(vin)))
	}
}

// handlePairingSignal reacts to one applied signed command.
//
// RACE NOTE. This runs on the reconcile loop's own goroutine, in the same
// select as the periodic tick, so it can never overlap a pass. A command
// applied WHILE a pass is running is not lost and not concurrent: it waits in
// the inbox and is handled the instant the pass ends, against the schedule row
// that pass just wrote. That ordering is why the reset is a single conditional
// UPDATE rather than a read-modify-write.
func (r *FleetConfigReconciler) handlePairingSignal(ctx context.Context, vin string) {
	now := r.now()
	c, found, err := r.deps.Pairing.ResetFleetConfigScheduleOnPairing(
		ctx, vin, now, now.Add(-pairingResetDebounce))
	if err != nil {
		r.logger.Warn("fleet-config reconcile: could not reset schedule on pairing",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()))
		return
	}
	if !found {
		// The overwhelmingly common case: the car is streaming fine and has no
		// schedule row at all. Debug, not Info — every command from every
		// healthy car lands here.
		r.logger.Debug("fleet-config reconcile: pairing signal has no schedule to reset",
			slog.String("vin", redactVIN(vin)))
		return
	}

	// THE HEADLINE FIX FOR HOLE 2. The backoff this car accumulated was earned
	// entirely by attempts made before the key existed; the evidence that it
	// exists now invalidates every one of them.
	r.logger.Info("fleet-config reconcile: virtual key proven paired by applied signed command — backoff reset",
		slog.String("event", "fleet_config_pairing_detected"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", redactVIN(vin)),
		slog.String("previous_outcome", c.LastOutcome))

	var out ReconcileOutcome
	out.Examined++
	r.reconcileOne(ctx, c, &out)
	r.logger.Info("fleet-config reconcile: pairing-triggered reconcile complete",
		slog.String("event", "fleet_config_pairing_reconcile"),
		slog.String("vehicle_id", c.VehicleID),
		slog.Int("repaired", out.Repaired),
		slog.Int("already_synced", out.AlreadySynced),
		slog.Int("forced_repushes", out.ForcedRepushes),
		slog.Int("awaiting_virtual_key", out.AwaitingKey))
}
