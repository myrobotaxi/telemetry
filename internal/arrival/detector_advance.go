// What one completed dwell MEANS, and what the detector does about it. Split
// from detector.go (which crossed the 300-line cap when MYR-539 gave a ride
// more than one waypoint to reach) along the seam that was already there: that
// file is the subscription, the frame path and the dwell bookkeeping, and this
// one is the single decision they exist to reach.

package arrival

import (
	"context"
	"errors"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// outcome is what one arrival attempt resolved to. Only outcomeUnavailable
// releases the latch.
type outcome int

const (
	// outcomeAdvanced: this detector won the guarded write and published.
	outcomeAdvanced outcome = iota
	// outcomeRefused: the write matched no row — the owner's tap won, or the
	// ride moved on. Silent and final.
	outcomeRefused
	// outcomeUnavailable: the write could not be completed (database error,
	// timeout, shutdown). Says nothing about the ride, so the latch is released
	// and a later frame may try again.
	outcomeUnavailable
)

// advance resolves one completed dwell. What that MEANS depends on the
// waypoint, and the asymmetry is the whole MYR-538 argument carried forward:
//
//   - PICKUP: run the guarded accepted → arrived transition and publish both
//     events. The status write is what a false positive could not be walked
//     back from, so it keeps every one of its guards.
//   - STOP / DESTINATION (MYR-539): publish the waypoint event and write NO
//     status. A stop's advance is the leg-advance consumer's business (it moves
//     stop statuses and re-points the dash, both reversible in practice), and
//     the DESTINATION deliberately completes nothing at all — dropped-off stays
//     the owner's tap, because ending a ride has consequences a wrong guess
//     cannot undo. The event exists there only so MYR-542 can flash the lights.
func (d *Detector) advance(cand Candidate, fix Fix) outcome {
	ctx, cancel := context.WithTimeout(d.ctx, d.cfg.Timeout)
	defer cancel()

	if !cand.isPickup() {
		d.logger.Info("auto-arrival: car reached a ride waypoint",
			slog.String("ride_id", cand.RideRequestID),
			slog.String("vehicle_id", cand.VehicleID),
			slog.String("vin", redactVIN(cand.VIN)),
			slog.String("waypoint", cand.Waypoint),
			slog.Duration("dwell", d.cfg.Dwell),
			slog.Float64("radius_meters", d.cfg.RadiusMeters),
			slog.Float64("car_miles_to_arrival", milesOrMissing(fix.MilesToArrival)),
		)
		d.publishWaypoint(ctx, cand)
		return outcomeAdvanced
	}

	updated, err := d.writer.MarkArrived(ctx, cand.RideRequestID)
	switch {
	case errors.Is(err, ErrRefused):
		// The expected loser's branch of the owner-tap race. Debug, because
		// nothing is wrong: the ride IS picked up, just not by us.
		d.logger.Debug("auto-arrival: transition refused, another writer won",
			slog.String("ride_id", cand.RideRequestID))
		return outcomeRefused
	case err != nil:
		d.logger.Warn("auto-arrival: transition failed",
			slog.String("ride_id", cand.RideRequestID),
			slog.String("error", err.Error()))
		return outcomeUnavailable
	}

	d.logger.Info("auto-arrival: car reached the pickup, ride advanced to arrived",
		slog.String("ride_id", cand.RideRequestID),
		slog.String("vehicle_id", cand.VehicleID),
		slog.String("vin", redactVIN(cand.VIN)),
		slog.Duration("dwell", d.cfg.Dwell),
		slog.Float64("radius_meters", d.cfg.RadiusMeters),
		// The car's own number, recorded as corroboration for whoever reads
		// this line later. It took no part in the decision — see the package
		// doc and MYR-527. -1 means the frame did not carry it.
		slog.Float64("car_miles_to_arrival", milesOrMissing(fix.MilesToArrival)),
	)

	d.publish(ctx, events.NewEvent(withPreviousStatus(updated)))
	d.publishWaypoint(ctx, cand)
	return outcomeAdvanced
}

// publishWaypoint emits the "the car itself is here" seam for whichever place
// the candidate was watching.
func (d *Detector) publishWaypoint(ctx context.Context, cand Candidate) {
	d.publish(ctx, events.NewEvent(events.RideWaypointArrivedEvent{
		RideRequestID: cand.RideRequestID,
		VehicleID:     cand.VehicleID,
		RiderID:       cand.RiderID,
		OwnerID:       cand.OwnerID,
		Waypoint:      cand.Waypoint,
	}))
}
