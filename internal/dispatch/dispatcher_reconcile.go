package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// InterruptedDispatchLister finds rides latched for dispatch (dispatched_at
// set) whose outcome never resolved (dispatch_status still NULL) and are
// older than olderThan — the orphan signature of a crash/SIGTERM in the
// claim→record window. Satisfied by the ride-request repo via a cmd/ adapter.
type InterruptedDispatchLister interface {
	ListInterruptedDispatches(ctx context.Context, olderThan time.Duration) ([]string, error)
}

// Reconcile resolves dispatches orphaned by a crash/SIGTERM in the window
// between ClaimDispatch and RecordDispatchOutcome. Such a ride sits with
// dispatched_at set and dispatch_status NULL forever — nothing else clears it,
// so it is stuck "claimed but unresolved" and invisible to monitoring.
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
	if olderThan < d.cfg.OverallTimeout {
		olderThan = d.cfg.OverallTimeout
	}
	ids, err := lister.ListInterruptedDispatches(ctx, olderThan)
	if err != nil {
		return 0, fmt.Errorf("dispatch reconcile: list interrupted: %w", err)
	}
	code := codeDispatchInterrupted
	resolved := 0
	for _, id := range ids {
		if err := d.store.RecordDispatchOutcome(ctx, id, OutcomeFailed, &code); err != nil {
			d.logger.Error("dispatch reconcile: failed to resolve interrupted dispatch",
				slog.String("ride_id", id),
				slog.String("error", err.Error()),
			)
			continue
		}
		resolved++
		d.logger.Info("dispatch reconcile: resolved interrupted dispatch",
			slog.String("ride_id", id),
			slog.String("outcome", string(OutcomeFailed)),
			slog.String("error_code", code),
		)
	}
	return resolved, nil
}
