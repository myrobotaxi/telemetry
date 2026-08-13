// The dispatcher's half of a trip edit (MYR-541): when the edited endpoint is
// the CURRENT leg's target, the car's dash is navigating to a place the ride
// no longer goes — so the new place is re-shared through the sequencer and
// then VERIFIED (MYR-527), which is what makes a mid-ride destination change
// trustworthy rather than hopeful.
//
// The decision table, over the ride's (unchanged) status at edit time:
//
//	pickup edited  + status accepted + leg-1 `sent` → re-share leg 1
//	dropoff edited + status enroute                 → re-share leg 2
//	everything else                                 → nothing to push
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
	if ev.NewDropoff != nil && ev.Status == "enroute" {
		d.reshareEditedLeg(ctx, dispatchLeg{
			name:      "dropoff",
			rideID:    ev.RideRequestID,
			vehicleID: ev.VehicleID,
			ownerID:   ev.OwnerID,
			coord:     *ev.NewDropoff,
			order:     legOrderDropoff,
		})
	}
}

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
