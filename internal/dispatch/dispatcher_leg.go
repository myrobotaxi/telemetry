// The shared nav-leg pipeline. Extracted from dispatcher.go (MYR-179) when
// that file passed the 300-line cap; behaviour is unchanged apart from the
// documented claim/post-claim split and the scheduled-accept guard.

package dispatch

import (
	"context"
	"errors"
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
	// order is the leg's monotonic position within the ride (MYR-526). It is
	// NOT an audit label: the nav sequencer uses it to keep the car's single
	// last-write-wins destination moving forwards only.
	order legOrder
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
		order:     legOrderPickup,
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
		order:     legOrderDropoff,
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
//
// LEG ORDERING (MYR-526). Everything from here to the Tesla call runs under the
// vehicle's nav gate, because the car's navigation destination is one
// last-write-wins resource and the two legs of a ride arrive as independent bus
// deliveries. Without the gate a stalled leg-1 push (an asleep car costs a wake
// plus a retry ladder) can land AFTER the leg-2 push that overtook it and drag
// the dash back to the pickup — with both legs recording `sent`, because both
// commands genuinely succeeded. See navSequencer for the full argument.
func (d *Dispatcher) runClaimedLeg(ctx context.Context, leg dispatchLeg) Outcome {
	if !d.cfg.Enabled {
		d.record(ctx, leg, "", OutcomeSkipped, nil, "")
		return OutcomeSkipped
	}

	// legCtx is cancelable so a later leg of this ride can take the gate away
	// mid-flight instead of waiting out this leg's wake/retry budget.
	legCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	hold, err := d.nav.acquire(legCtx, leg.vehicleID, leg.rideID, leg.order, cancel)
	if err != nil {
		if errors.Is(err, errNavSuperseded) {
			// A later leg of this ride already reached the car. Pushing now
			// would walk the destination backwards, so we do not — and we say
			// so on the row and in the log rather than dropping it silently.
			code := codeNavSuperseded
			d.record(ctx, leg, "", OutcomeSkipped, &code, "")
			return OutcomeSkipped
		}
		code := codeCanceled
		d.record(ctx, leg, "", OutcomeFailed, &code, "")
		return OutcomeFailed
	}
	defer hold.Release()

	vin, code := d.resolveVIN(legCtx, leg.vehicleID)
	if code != nil {
		return d.recordSequenced(ctx, leg, hold, "", OutcomeFailed, code, "")
	}

	token, code := d.resolveToken(legCtx, leg.ownerID)
	if code != nil {
		return d.recordSequenced(ctx, leg, hold, vin, OutcomeFailed, code, "")
	}

	outcome, ecode, detail := d.executeWithRetry(legCtx, vin, token, leg.coord)
	return d.recordSequenced(ctx, leg, hold, vin, outcome, ecode, detail)
}

// recordSequenced persists a sequenced leg's outcome, reclassifying a failure
// that was CAUSED by supersession. A leg the sequencer cancelled mid-flight did
// not fail against Tesla and was not merely "canceled" — it was deliberately
// stopped because the ride had already moved to its next target. Recording it
// as failed/dispatch_canceled would put a phantom failure on a ride that is
// working perfectly, and would hide the one fact worth knowing: the later leg
// won. Records on the OUTER ctx, whose deadline `record` drops anyway, so the
// write survives the cancellation that produced this outcome.
//
// A leg that resolved OutcomeSent is deliberately NOT reclassified, even when
// the supersession flag is set: in that race the command had already reached
// the car, so `sent` is the true statement about what happened. The ORDERING
// guarantee is unaffected — the superseding leg cannot dial until it takes the
// gate this one still holds, so it lands after, which is the whole point.
func (d *Dispatcher) recordSequenced(
	ctx context.Context, leg dispatchLeg, hold *navHold, vin string, outcome Outcome, code *string, detail string,
) Outcome {
	if outcome != OutcomeSent && hold.Superseded() {
		superseded := codeNavSuperseded
		d.record(ctx, leg, vin, OutcomeSkipped, &superseded, "")
		return OutcomeSkipped
	}
	d.record(ctx, leg, vin, outcome, code, detail)
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
