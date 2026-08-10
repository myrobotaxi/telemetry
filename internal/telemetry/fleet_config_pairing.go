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

// pairingExamDedupe is the minimum gap between two IMMEDIATE examinations of
// the same vehicle, and it is what keeps MYR-529's widening of the trigger
// surface from multiplying Tesla traffic.
//
// Three doors now hand the reconciler the same fact — the signed-command
// observer, §7.23's `fleet_status` paired answer, and a link-time push that
// APPLIED — and an owner finishing setup can plausibly trip all three inside a
// few seconds. The reset itself is already debounced at ten minutes, but the
// reset is not the expensive half; the examination is. One per minute per
// vehicle, evaluated on the loop goroutine so it needs no lock.
const pairingExamDedupe = time.Minute

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

// PairingEvidence is the ONE entry point every door uses to tell the reconciler
// that a vehicle's virtual key is proven paired (MYR-529).
//
// SignedCommandApplied is its original name and remains the
// commands.SignedCommandObserver implementation; this alias exists so the
// §7.23 completion sequence and the link-time provisioning hook can say what
// they actually observed — Tesla's `fleet_status` answer, and a config push
// that APPLIED — without either of them pretending to be a signed command.
// Same inbox, same debounce, same dedupe: N signals for one car in one minute
// produce exactly one examination.
func (r *FleetConfigReconciler) PairingEvidence(vin string) { r.SignedCommandApplied(vin) }

// recentlyExamined reports whether this VIN has had an immediate examination
// inside pairingExamDedupe, and records the attempt when it has not.
//
// Called only from handlePairingSignal, which runs on the loop goroutine, so
// the map is single-threaded by construction. Pruned on every call, which keeps
// it bounded by the number of DISTINCT vehicles signalling within one window
// rather than by uptime.
func (r *FleetConfigReconciler) recentlyExamined(vin string) bool {
	now := r.now()
	for k, at := range r.examined {
		if now.Sub(at) >= pairingExamDedupe {
			delete(r.examined, k)
		}
	}
	if at, ok := r.examined[vin]; ok && now.Sub(at) < pairingExamDedupe {
		return true
	}
	r.examined[vin] = now
	return false
}

// handlePairingSignal reacts to one piece of pairing evidence.
//
// RACE NOTE. This runs on the reconcile loop's own goroutine, in the same
// select as the periodic tick, so it can never overlap a pass. A command
// applied WHILE a pass is running is not lost and not concurrent: it waits in
// the inbox and is handled the instant the pass ends, against the schedule row
// that pass just wrote. That ordering is why the reset is a single conditional
// UPDATE rather than a read-modify-write.
func (r *FleetConfigReconciler) handlePairingSignal(ctx context.Context, vin string) {
	if r.recentlyExamined(vin) {
		r.logger.Debug("fleet-config reconcile: pairing signal deduped (this vehicle was just examined)",
			slog.String("vin", redactVIN(vin)))
		return
	}

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
		// Now only two ways to get here (MYR-517 made "no schedule row" a
		// create-and-proceed): the VIN is not ours, or this car already opened a
		// pairing epoch inside the debounce window. Debug, not Info — every
		// command from every healthy car lands in the second case.
		r.logger.Debug("fleet-config reconcile: pairing signal not actionable (unknown VIN or debounced)",
			slog.String("vin", redactVIN(vin)))
		return
	}

	// THE HEADLINE FIX FOR HOLE 2. The backoff this car accumulated was earned
	// entirely by attempts made before the key existed; the evidence that it
	// exists now invalidates every one of them.
	//
	// THE MYR-517 ARM USED TO RETURN HERE, and MYR-529 lets it fall through.
	// Its objection was sound at the time: a car we have never recorded a
	// failed attempt against is overwhelmingly likely to be simply streaming,
	// and turning every first command from every healthy car into a Tesla
	// config read would have been a worse bug than the one it fixed. What has
	// changed is that "is this car streaming?" is now answered for FREE, from
	// the very row this reset just returned — `status` plus `lastUpdated`, no
	// upstream call — so the healthy majority exits reconcileOne at zero cost
	// and the objection no longer applies. What is left is the case MYR-517
	// wrote the seed FOR: a car that is not streaming, whose key is proven
	// paired, and which used to wait out the staleness rule before anyone
	// looked at it. Those get looked at now.
	r.logger.Info("fleet-config reconcile: virtual key proven paired — backoff reset, examining now",
		slog.String("event", "fleet_config_pairing_detected"),
		slog.String("vehicle_id", c.VehicleID),
		slog.String("vin", redactVIN(vin)),
		slog.Bool("schedule_created", c.ScheduleCreated),
		slog.String("previous_outcome", c.LastOutcome))

	var out ReconcileOutcome
	out.Examined++
	r.reconcileOne(ctx, c, &out)
	r.logger.Info("fleet-config reconcile: pairing-triggered reconcile complete",
		slog.String("event", "fleet_config_pairing_reconcile"),
		slog.String("vehicle_id", c.VehicleID),
		slog.Int("repaired", out.Repaired),
		slog.Int("already_streaming", out.AlreadyStreaming),
		slog.Int("already_synced", out.AlreadySynced),
		slog.Int("forced_repushes", out.ForcedRepushes),
		slog.Int("awaiting_virtual_key", out.AwaitingKey))
}
