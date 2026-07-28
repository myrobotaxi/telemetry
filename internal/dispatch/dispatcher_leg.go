// The shared nav-leg pipeline. Extracted from dispatcher.go (MYR-179) when
// that file passed the 300-line cap; behaviour is unchanged apart from the
// documented claim/post-claim split and the scheduled-accept guard.

package dispatch

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// dispatchLeg is one nav push. The legs — pickup (on accept for an instant
// ride, at `scheduledFor` for a reservation — MYR-179) and dropoff (on rider
// start, MYR-270) — share the whole pipeline (claim → resolve → command →
// record) and differ ONLY in: the exactly-once latch to claim, the coordinate
// to push, the outcome column to write, and a label for the audit line.
// process, processDropoff, and the reservation sweeper build a leg and hand it
// to runLeg (or, when the caller has already won the claim itself,
// runClaimedLeg).
type dispatchLeg struct {
	name      string // "pickup" | "dropoff" — audit label only
	rideID    string
	vehicleID string
	ownerID   string
	coord     events.RidePlace
	claim     func(context.Context, string) (bool, error)
	record    func(context.Context, string, Outcome, *string) error
}

// process runs the leg-1 (pickup) dispatch for one accepted ride. It is safe to
// call directly in tests.
//
// SCHEDULED rides do NOT dispatch here (MYR-179). Accepting a reservation is
// not dispatching it: the pickup nav must reach the car at `scheduledFor`, not
// hours or days early, so a scheduled accept returns without touching the
// latch. The row is left EXACTLY as an un-accepted instant ride would be —
// dispatched_at NULL, dispatch_status NULL (absent = pending, the same shape a
// pre-dispatch instant ride has) — which is precisely what makes it selectable
// by the reservation sweeper later. Deliberately NOT recorded as `skipped`:
// `skipped` is the kill-switch outcome and means "we decided not to push this
// ride at all", which would be a lie about a reservation that is going to
// dispatch on schedule, and it would also latch the row out of the sweep.
//
// The guard reads the event's own ScheduledFor (carried since MYR-175) rather
// than re-reading the row: the accept handler projects it from the winning
// write, so it is the authoritative value for THIS accept.
func (d *Dispatcher) process(ctx context.Context, ev events.RideAcceptedEvent) {
	if ev.ScheduledFor != nil {
		d.logger.Info("dispatch: scheduled ride, deferring pickup to reservation time",
			slog.String("leg", "pickup"),
			slog.String("ride_id", ev.RideRequestID),
			slog.String("vehicle_id", ev.VehicleID),
			slog.Time("scheduled_for", *ev.ScheduledFor),
		)
		return
	}
	d.runLeg(ctx, d.pickupLeg(ev.RideRequestID, ev.VehicleID, ev.OwnerID, ev.Pickup))
}

// pickupLeg builds the leg-1 (pickup) leg. Shared by the instant path (accept)
// and the reservation sweeper (MYR-179) so both claim the SAME dispatched_at
// latch and write the SAME dispatch_status column — scheduled dispatch
// inherits leg-1's exactly-once and restart-safe semantics rather than
// re-deriving them.
func (d *Dispatcher) pickupLeg(rideID, vehicleID, ownerID string, pickup events.RidePlace) dispatchLeg {
	return dispatchLeg{
		name:      "pickup",
		rideID:    rideID,
		vehicleID: vehicleID,
		ownerID:   ownerID,
		coord:     pickup,
		claim:     d.store.ClaimDispatch,
		record:    d.store.RecordDispatchOutcome,
	}
}

// processDropoff runs the leg-2 (dropoff) dispatch for one started ride
// (MYR-270): identical pipeline to process, claiming/recording the independent
// dropoff_* columns and pushing the DROPOFF coordinate. Safe to call in tests.
func (d *Dispatcher) processDropoff(ctx context.Context, ev events.RideStartedEvent) {
	d.runLeg(ctx, dispatchLeg{
		name:      "dropoff",
		rideID:    ev.RideRequestID,
		vehicleID: ev.VehicleID,
		ownerID:   ev.OwnerID,
		coord:     ev.Dropoff,
		claim:     d.store.ClaimDropoffDispatch,
		record:    d.store.RecordDropoffDispatchOutcome,
	})
}

// runLeg claims the leg's exactly-once latch and, if it wins, runs the push.
// Shared by both legs; the leg struct supplies the per-leg claim/record seams
// and the coordinate.
func (d *Dispatcher) runLeg(ctx context.Context, leg dispatchLeg) {
	claimed, err := leg.claim(ctx, leg.rideID)
	if err != nil {
		// Could not claim safely — do not push nav (we cannot guarantee
		// exactly-once). Log and drop; the leg stays un-dispatched.
		d.logger.Error("dispatch: claim failed",
			slog.String("leg", leg.name),
			slog.String("ride_id", leg.rideID),
			slog.String("vehicle_id", leg.vehicleID),
			slog.String("error", err.Error()),
		)
		return
	}
	if !claimed {
		// Already dispatched by a prior delivery — exactly-once guard.
		d.logger.Debug("dispatch: leg already dispatched, skipping",
			slog.String("leg", leg.name),
			slog.String("ride_id", leg.rideID),
		)
		return
	}
	_ = d.runClaimedLeg(ctx, leg)
}

// runClaimedLeg is the post-claim half of the pipeline: (kill-switch | resolve
// → command) → record. The caller MUST already hold the leg's claim. It
// returns the RESOLVED outcome (the same value it just recorded).
//
// Split out from runLeg by MYR-179 so the reservation sweeper can interpose
// between the claim and the push — it must evaluate expiry and the
// vehicle-busy hold BEFORE claiming — while still running the identical
// resolve/retry/record machinery. The sweeper reuses this rather than forking
// it, so a change to the retry policy or the outcome contract lands on both
// paths at once.
//
// The returned outcome exists for the sweeper's `ride.due` seam: that event
// means "your car is on the way", so it may only fire once the push actually
// resolved `sent`. The instant path ignores the return value — its outcome is
// already persisted and has no downstream seam.
func (d *Dispatcher) runClaimedLeg(ctx context.Context, leg dispatchLeg) Outcome {
	if !d.cfg.Enabled {
		d.record(ctx, leg, "", OutcomeSkipped, nil, "")
		return OutcomeSkipped
	}

	vin, code := d.resolveVIN(ctx, leg.vehicleID)
	if code != nil {
		d.record(ctx, leg, "", OutcomeFailed, code, "")
		return OutcomeFailed
	}

	token, code := d.resolveToken(ctx, leg.ownerID)
	if code != nil {
		d.record(ctx, leg, vin, OutcomeFailed, code, "")
		return OutcomeFailed
	}

	outcome, ecode, detail := d.executeWithRetry(ctx, vin, token, leg.coord)
	d.record(ctx, leg, vin, outcome, ecode, detail)
	return outcome
}

// record persists the outcome and emits the single per-attempt audit line.
// The write runs on a context DETACHED from the per-event ctx (which bounds
// the whole dispatch and may already be canceled/timed-out — precisely when
// we most need to persist the outcome). Without WithoutCancel a timed-out
// ride would stay claimed (dispatched_at set) with a NULL dispatch_status
// forever; the startup reconciler would then have to clean it up. We keep the
// ctx values but drop its deadline, adding our own short bound.
// detail is the opaque Tesla-side reason (e.g. `invalid_command`) surfaced on
// the audit line as error_detail. It is empty for non-command outcomes and is
// NOT persisted (no DB column — the detail lives only in the structured log).
func (d *Dispatcher) record(ctx context.Context, leg dispatchLeg, vin string, outcome Outcome, code *string, detail string) {
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := leg.record(recCtx, leg.rideID, outcome, code); err != nil {
		d.logger.Error("dispatch: failed to record outcome",
			slog.String("leg", leg.name),
			slog.String("ride_id", leg.rideID),
			slog.String("outcome", string(outcome)),
			slog.String("error", err.Error()),
		)
	}

	attrs := []any{
		slog.String("leg", leg.name),
		slog.String("ride_id", leg.rideID),
		slog.String("vehicle_id", leg.vehicleID),
		slog.String("vin", redactVIN(vin)),
		slog.String("outcome", string(outcome)),
	}

	if code != nil {
		attrs = append(attrs, slog.String("error_code", *code))
	}
	if detail != "" {
		attrs = append(attrs, slog.String("error_detail", detail))
	}
	d.logger.Info("dispatch attempt", attrs...)
}
