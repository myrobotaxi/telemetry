package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// InterruptedDispatchLister finds LEG-1 (pickup) dispatches latched
// (dispatched_at set) whose outcome never resolved (dispatch_status still NULL)
// and are older than olderThan — the orphan signature of a crash/SIGTERM in the
// claim→record window. Satisfied by the ride-request repo via a cmd/ adapter.
type InterruptedDispatchLister interface {
	ListInterruptedDispatches(ctx context.Context, olderThan time.Duration) ([]string, error)
}

// InterruptedDropoffDispatchLister is the LEG-2 (dropoff) analogue of
// InterruptedDispatchLister (MYR-266): it finds dropoff dispatches latched
// (dropoff_dispatched_at set) whose outcome never resolved
// (dropoff_dispatch_status still NULL) and older than olderThan. Migration 0007
// deliberately shipped WITHOUT this leg-2 reconciler ("a follow-up can extend
// the reconciler if leg-2 interruptions prove material"); MYR-266 adds it so a
// dropoff push orphaned by a crash is no longer stuck claimed-but-unresolved.
// Satisfied by the ride-request repo via a cmd/ adapter.
type InterruptedDropoffDispatchLister interface {
	ListInterruptedDropoffDispatches(ctx context.Context, olderThan time.Duration) ([]string, error)
}

// Reconcile resolves LEG-1 (pickup) dispatches orphaned by a crash/SIGTERM in
// the window between ClaimDispatch and RecordDispatchOutcome. Such a ride sits
// with dispatched_at set and dispatch_status NULL forever — nothing else clears
// it, so it is stuck "claimed but unresolved" and invisible to monitoring.
//
// We record each as failed / dispatch_interrupted rather than re-dispatching.
// Rationale: the process died at an UNKNOWN point, so the nav push may or may
// not have reached the vehicle; the accept may be minutes/hours stale by the
// time we restart; and pushing nav to a car that has since moved on is worse
// than an honest, alertable "interrupted" outcome. The exactly-once latch has
// already done its job — reconciliation only needs to unstick the NULL status.
// (See rest-api.md §7.8 for the recorded-code contract.)
//
// olderThan is floored at OverallTimeout so a genuinely in-flight dispatch
// (which briefly has the same dispatched_at-set / status-NULL shape) is never
// stomped. Returns the count of rides resolved.
func (d *Dispatcher) Reconcile(ctx context.Context, lister InterruptedDispatchLister, olderThan time.Duration) (int, error) {
	return d.reconcileLeg(ctx, "pickup", lister.ListInterruptedDispatches, d.store.RecordDispatchOutcome, olderThan)
}

// ReconcileDropoff is the LEG-2 (dropoff) analogue of Reconcile (MYR-266): it
// resolves dropoff dispatches orphaned by a crash/SIGTERM in the window between
// ClaimDropoffDispatch and RecordDropoffDispatchOutcome (dropoff_dispatched_at
// set, dropoff_dispatch_status NULL). Identical honest-outcome semantics: each
// interrupted dropoff is recorded failed / dispatch_interrupted, NOT re-pushed
// — restarting minutes/hours later and dialing a stale dropoff to a car whose
// rider may already be gone is worse than an alertable "interrupted" outcome.
//
// Idempotency: the lister matches ONLY the claimed-but-unresolved shape
// (dropoff_dispatched_at IS NOT NULL AND dropoff_dispatch_status IS NULL). A
// dropoff that RESOLVED — 'sent', 'failed', or 'skipped' — has a non-NULL
// status and is never selected, so a car that already received its dropoff nav
// is never touched and no push is ever duplicated. The age floor keeps a live
// in-flight dropoff from being mistaken for an orphan.
func (d *Dispatcher) ReconcileDropoff(ctx context.Context, lister InterruptedDropoffDispatchLister, olderThan time.Duration) (int, error) {
	return d.reconcileLeg(ctx, "dropoff", lister.ListInterruptedDropoffDispatches, d.store.RecordDropoffDispatchOutcome, olderThan)
}

// reconcileLeg is the shared reconciliation body for both nav legs (MYR-266):
// it lists interrupted dispatches for the leg, then records each as
// failed / dispatch_interrupted via the leg's own outcome-write seam. The two
// legs differ ONLY in the lister and the recorder (independent columns), so the
// generalization mirrors the runLeg dispatch pipeline. Records on the passed
// ctx (the caller bounds the one-shot startup pass). Returns the count resolved.
func (d *Dispatcher) reconcileLeg(
	ctx context.Context,
	leg string,
	list func(context.Context, time.Duration) ([]string, error),
	record func(context.Context, string, Outcome, *string) error,
	olderThan time.Duration,
) (int, error) {
	if olderThan < d.cfg.OverallTimeout {
		olderThan = d.cfg.OverallTimeout
	}
	ids, err := list(ctx, olderThan)
	if err != nil {
		return 0, fmt.Errorf("dispatch reconcile (%s): list interrupted: %w", leg, err)
	}
	code := codeDispatchInterrupted
	resolved := 0
	for _, id := range ids {
		if err := record(ctx, id, OutcomeFailed, &code); err != nil {
			d.logger.Error("dispatch reconcile: failed to resolve interrupted dispatch",
				slog.String("leg", leg),
				slog.String("ride_id", id),
				slog.String("error", err.Error()),
			)
			continue
		}
		resolved++
		d.logger.Info("dispatch reconcile: resolved interrupted dispatch",
			slog.String("leg", leg),
			slog.String("ride_id", id),
			slog.String("outcome", string(OutcomeFailed)),
			slog.String("error_code", code),
		)
	}
	return resolved, nil
}
