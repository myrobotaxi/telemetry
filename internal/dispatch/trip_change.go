// The dispatcher's half of a trip edit (MYR-541): when the edited endpoint is
// the CURRENT leg's target, the car's dash is navigating to a place the ride
// no longer goes — so the new place is re-shared through the sequencer and
// then VERIFIED (MYR-527), which is what makes a mid-ride destination change
// trustworthy rather than hopeful.
//
// The decision table, over the ride's (unchanged) status at edit time:
//
//	pickup edited  + status accepted + leg-1 `sent` → re-share leg 1
//	stops edited   + status enroute                 → re-share leg 2 (LegTarget)
//	dropoff edited + status enroute                 → re-share leg 2
//	everything else                                 → nothing to push
//
// The stops arm SUPERSEDES the dropoff arm rather than adding to it (MYR-539):
// once a trip has stops, the current leg's target is the current stop, and the
// server states it on LegTarget — which for a trip whose stops were all cleared
// is the (possibly just-edited) drop-off. One edit is one re-share; two arms
// firing would race two pushes for one decision.
//
// A drop-off edited before the rider's Start needs no push at all: leg 2 has
// not dispatched, and the Start will dispatch the NEW drop-off for free. A
// pickup edited on an undispatched reservation likewise — the sweeper re-reads
// the pickup every tick, so the MYR-535 lead re-computes on its own.

package dispatch

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// handleTripChanged is the events.Handler for ride.trip_changed.
func (d *Dispatcher) handleTripChanged(evt events.Event) {
	ev, ok := evt.Payload.(events.RideTripChangedEvent)
	if !ok {
		d.logger.Error("dispatch: unexpected payload type on ride.trip_changed",
			slog.String("event_id", evt.ID),
		)
		return
	}
	d.dispatchAsync(func(ctx context.Context) { d.processTripChanged(ctx, ev) })
}

// processTripChanged runs the decision table. Each qualifying leg re-shares
// via reshareNav — reacquire admits the leg's own order, a later leg still
// wins, and neither the claim nor the outcome row is touched — then hands the
// NEW coordinate to the MYR-527 verifier. A re-share that FAILED outright
// reports through the verifier's own unapplied seam: the dash provably holds
// the old target, which is exactly what the owner's "check the dash" push is
// for.
func (d *Dispatcher) processTripChanged(ctx context.Context, ev events.RideTripChangedEvent) {
	if ev.NewPickup != nil && ev.Status == "accepted" && ev.PickupDispatched {
		d.reshareEditedLeg(ctx, dispatchLeg{
			name:      "pickup",
			rideID:    ev.RideRequestID,
			vehicleID: ev.VehicleID,
			ownerID:   ev.OwnerID,
			coord:     *ev.NewPickup,
			order:     legOrderPickup,
		})
	}
	if ev.Status != statusEnroute {
		return
	}
	switch {
	case ev.StopsChanged && ev.LegTarget != nil:
		// The stop list moved, so the CURRENT leg's target may have moved with
		// it even though neither endpoint did. The server already resolved
		// which place that is.
		d.reshareEditedLeg(ctx, dispatchLeg{
			name:      "stop",
			rideID:    ev.RideRequestID,
			vehicleID: ev.VehicleID,
			ownerID:   ev.OwnerID,
			coord:     *ev.LegTarget,
			order:     legOrderDropoff,
		})
	case ev.NewDropoff != nil:
		d.reshareEditedLeg(ctx, dispatchLeg{
			name:      "dropoff",
			rideID:    ev.RideRequestID,
			vehicleID: ev.VehicleID,
			ownerID:   ev.OwnerID,
			coord:     *ev.NewDropoff,
			order:     legOrderDropoff,
		})
	case ev.StopsChanged:
		// The publisher computes LegTarget for every stops edit, so reaching
		// here means that invariant broke — a future publisher, a bad merge.
		// The consequence is a car still driving to a stop the trip no longer
		// has, which is exactly the silence this line refuses to keep.
		d.logger.Error("dispatch: stops edit carried no leg target, nav not re-shared",
			slog.String("ride_id", ev.RideRequestID),
			slog.String("vehicle_id", ev.VehicleID),
		)
	}
}

// statusEnroute is the ride status in which leg 2's target is the car's
// rightful destination — the only one in which a post-pickup re-share means
// anything.
const statusEnroute = "enroute"

// reshareEditedLeg pushes an edited endpoint and arms verification.
func (d *Dispatcher) reshareEditedLeg(ctx context.Context, leg dispatchLeg) {
	switch d.reshareNav(ctx, leg) {
	case reshareSent:
		// Verified like any share (MYR-527): observe the car's own reported
		// destination converge on the NEW target; one more re-share; then the
		// owner's seam.
		d.armNavVerify(leg)
	case reshareFailed:
		// The push itself did not deliver — the dash still holds the OLD
		// target. The verifier's report path already says the honest thing
		// and applies its own still-current check.
		if v := d.verify; v != nil {
			d.reportNavUnapplied(ctx, v, leg)
		}
	case reshareAbandoned:
		// Superseded or shutting down; someone else owns the situation.
	}
}
