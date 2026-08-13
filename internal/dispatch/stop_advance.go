// LEG ADVANCE on a multi-stop trip (MYR-539): the car reached a STOP, so the
// stop is behind it and the dash must be pointed at the next one.
//
// The trigger is the MYR-538 waypoint seam — "the car itself is here, right
// now, and we know it from telemetry" — which is the only evidence that may
// move a leg forward. A person's tap cannot produce it, and neither can the
// car's own dash reading (MYR-527 is the standing proof that the dash's target
// and the ride's target are different facts).
//
// TWO THINGS HAPPEN, IN THIS ORDER, AND ONLY IN THIS ORDER:
//
//  1. the guarded write marks the arrived stop completed and promotes the next
//     upcoming one to current, exactly once, committed; then
//  2. the car's nav is RE-SHARED to whatever that write says is next — the new
//     current stop, or the final drop-off — through the same sequencer gate and
//     the same MYR-527 verifier every other share goes through.
//
// The order is the exactly-once story: only the process that WON the write
// re-shares, so a re-delivered waypoint event (or a second replica seeing the
// same arrival) re-dials nothing. It also means a re-share is never sent on a
// leg the database refused to advance.
//
// The leg keeps legOrderDropoff. Every post-pickup target of a ride is one leg
// as far as MYR-526's ordering is concerned: the sequencer's job is to stop a
// stalled leg-1 push from landing after a leg-2 push, and stop-to-stop shares
// are the SAME leg walking forward, arbitrated by the reacquire rule that
// already admits a leg's own order.

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// ErrStopNotAdvanced is the ordinary loser's outcome: the stop was already
// completed (a re-delivered event) or does not belong to the ride. The
// dispatcher no-ops silently on it — somebody already advanced this leg.
var ErrStopNotAdvanced = errors.New("dispatch: ride stop already advanced")

// StopAdvance is what one won advance resolved to: where the car goes next.
type StopAdvance struct {
	// NextStopID is the stop that became current, or empty when the completed
	// stop was the last one and the final drop-off is next.
	NextStopID string
	// NextTarget is the place to re-share. P1 — never logged.
	NextTarget events.RidePlace
}

// StopStore is the leg-advance's write seam, satisfied by the ride-request repo
// through a cmd/ adapter. Consumer-site per convention.
type StopStore interface {
	// AdvanceStopArrival marks the arrived stop completed, promotes the next
	// upcoming stop to current, and returns the ride's next target — one
	// transaction, exactly-once. ErrStopNotAdvanced when the guard refused.
	AdvanceStopArrival(ctx context.Context, rideID, stopID string) (StopAdvance, error)
}

// WithStopAdvance wires the multi-stop leg advance. Nil store = off, which is
// the ordinary state for every test and for a deploy that wires nothing: the
// subscription still exists, and every waypoint event is then a no-op.
func (d *Dispatcher) WithStopAdvance(store StopStore) *Dispatcher {
	d.stops = store
	return d
}

// handleWaypointArrived is the events.Handler for ride.waypoint_arrived. Only
// an intermediate STOP advances anything: the pickup's own lifecycle
// transition already travelled on the status event, and the destination
// deliberately advances nothing at all — the ride ends on the owner's tap, and
// this seam exists there only so MYR-542 can flash the lights.
func (d *Dispatcher) handleWaypointArrived(evt events.Event) {
	ev, ok := evt.Payload.(events.RideWaypointArrivedEvent)
	if !ok {
		d.logger.Error("dispatch: unexpected payload type on ride.waypoint_arrived",
			slog.String("event_id", evt.ID),
		)
		return
	}
	stopID, isStop := events.StopIDFromWaypoint(ev.Waypoint)
	if !isStop || d.stops == nil {
		return
	}
	d.dispatchAsync(func(ctx context.Context) { d.processStopArrival(ctx, ev, stopID) })
}

// processStopArrival runs the guarded advance and, on a win, re-shares the next
// target. A refusal is silent by design; a failure is loud and changes nothing,
// because the car is still navigating to a stop it has already reached and the
// owner can see that on the dash.
func (d *Dispatcher) processStopArrival(ctx context.Context, ev events.RideWaypointArrivedEvent, stopID string) {
	advance, err := d.stops.AdvanceStopArrival(ctx, ev.RideRequestID, stopID)
	switch {
	case errors.Is(err, ErrStopNotAdvanced):
		// Re-delivery, or another replica got there first. Debug, because
		// nothing is wrong: the leg IS advanced, just not by us — and the
		// winner has already re-shared.
		d.logger.Debug("dispatch: stop already advanced, skipping re-share",
			slog.String("ride_id", ev.RideRequestID),
		)
		return
	case err != nil:
		d.logger.Error("dispatch: stop advance failed",
			slog.String("ride_id", ev.RideRequestID),
			slog.String("error", err.Error()),
		)
		return
	}

	d.logger.Info("dispatch: car reached a stop, advancing to the next target",
		slog.String("ride_id", ev.RideRequestID),
		slog.String("vehicle_id", ev.VehicleID),
		slog.Bool("next_is_dropoff", advance.NextStopID == ""),
	)
	d.reshareEditedLeg(ctx, dispatchLeg{
		name:      stopLegName(advance.NextStopID),
		rideID:    ev.RideRequestID,
		vehicleID: ev.VehicleID,
		ownerID:   ev.OwnerID,
		coord:     advance.NextTarget,
		order:     legOrderDropoff,
	})
}

// stopLegName is the audit label for the leg the car is now driving. It names
// what the target IS rather than a position in the list — a log line that says
// "stop" or "dropoff" survives the list being edited under it.
func stopLegName(nextStopID string) string {
	if nextStopID == "" {
		return "dropoff"
	}
	return "stop"
}

// subscribeWaypoints registers the leg-advance handler. Called from Subscribe
// alongside the other seams; kept here so the whole multi-stop wiring reads as
// one unit.
func (d *Dispatcher) subscribeWaypoints(bus events.Bus) error {
	if _, err := bus.Subscribe(events.TopicRideWaypointArrived, d.handleWaypointArrived); err != nil {
		return fmt.Errorf("dispatch.Subscribe(waypoint_arrived): %w", err)
	}
	return nil
}
