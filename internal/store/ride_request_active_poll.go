// Active-ride poll targets for the MYR-394 ride-scoped position poller.
//
// The poller needs exactly one thing from the database: "which vehicles are
// mid-ride right now, and what are their VINs". It asks at STARTUP (and on a
// slow safety-net cadence) so that a poller can never outlive the ride that
// justified it — the same reconcile shape the MYR-176 dispatch retry sweep and
// the MYR-266 reservation sweeper use, for the same reason: a process restart
// loses every in-memory poller, and a dropped bus event loses one.

package store

import (
	"context"
	"fmt"
	"time"
)

// pollableRidePredicate is the single definition of "this car is carrying out a
// ride RIGHT NOW, so its position is worth a REST read".
//
// It is deliberately WIDER than activeInstantRidePredicate (the `hasActiveRide`
// busy flag) in exactly one direction — scheduled rides — and the difference is
// the whole design:
//
//   - An INSTANT ride at accepted/enroute/arrived is live by definition: the
//     owner accepted seconds ago and the rider is watching the map. Same three
//     states as the busy predicate.
//   - A SCHEDULED ride at `accepted` is NOT live. It may be three days out.
//     Polling it would burn the Fleet API budget for a car that is parked in a
//     driveway with nobody looking at it, which is the one failure mode this
//     feature must not introduce.
//   - A scheduled ride BECOMES live at one of two observable moments: the
//     reservation sweeper delivers its pickup nav push (dispatch_status =
//     'sent', the same instant TopicRideDue is published, MYR-179), or the rider
//     starts it (enroute/arrived). Either makes the car a real vehicle on a real
//     leg with a rider tracking it.
//
// Terminal states (completed / cancelled / declined) and `requested` are
// excluded: nothing is under way, so there is nothing to track.
const pollableRidePredicate = `r.status IN ('accepted', 'enroute', 'arrived')
		  AND (
		    r.scheduled_for IS NULL
		    OR r.status IN ('enroute', 'arrived')
		    OR r.dispatch_status = 'sent'
		  )`

// queryActiveRidePollTargets lists the (ride, vehicle, VIN) triples the poller
// should currently hold a poller for.
//
// The join onto the Prisma-owned "Vehicle" table is a READ, which needs no
// carve-out (data-lifecycle.md §1.4 constrains WRITES). Rows with an empty VIN
// are excluded for the same reason ListInServiceVINs excludes them: the MYR-257
// provisioning INSERT can seed a placeholder row before Tesla has supplied a
// VIN, and there is nothing to read from the Fleet API for one.
//
// Ordered oldest-updated first so that when a pass is truncated by the limit it
// sheds the most recently touched rides, never the most neglected.
const queryActiveRidePollTargets = `
SELECT r.id, r.vehicle_id, v."vin"
FROM go_ride_requests r
JOIN "Vehicle" v ON v."id" = r.vehicle_id
WHERE ` + pollableRidePredicate + `
  AND v."vin" <> ''
ORDER BY r.updated_at ASC
LIMIT $1`

// ActiveRidePollTarget is one vehicle that should be position-polled, together
// with the ride that justifies it.
//
// Deliberately NOT a RideRequestRecord: the poller has no use for pickup or
// dropoff, and returning a full record would mean decrypting P1 coordinates for
// no reason on every reconcile pass.
type ActiveRidePollTarget struct {
	// RideRequestID is the ride whose lifetime bounds the poller.
	RideRequestID string
	// VehicleID is the ride's vehicle (the Prisma "Vehicle"."id").
	VehicleID string
	// VIN is the Fleet API key for the vehicle_data read. P1 — log redacted.
	VIN string
}

// ListActiveRidePollTargets returns up to limit vehicles currently mid-ride.
//
// Backs the MYR-394 reconcile: the poller registry is rebuilt from this list, so
// a ride that ended while the process was down (or whose terminal event was
// dropped) has its poller reaped, and a ride that started while the process was
// down gets one adopted.
//
// A non-positive limit returns no rows and no error, matching
// ListInServiceVINs: the caller's config defaults already guarantee a positive
// cap, and refusing to run an unbounded scan is the safer reading of a zero.
func (r *RideRequestRepo) ListActiveRidePollTargets(ctx context.Context, limit int) ([]ActiveRidePollTarget, error) {
	if limit <= 0 {
		return nil, nil
	}
	const op = "ride_request.list_active_poll_targets"
	start := time.Now()

	rows, err := r.pool.Query(ctx, queryActiveRidePollTargets, limit)
	if err != nil {
		r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("RideRequestRepo.ListActiveRidePollTargets: query: %w", err)
	}
	defer rows.Close()

	targets := make([]ActiveRidePollTarget, 0, limit)
	for rows.Next() {
		var t ActiveRidePollTarget
		if err := rows.Scan(&t.RideRequestID, &t.VehicleID, &t.VIN); err != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("RideRequestRepo.ListActiveRidePollTargets: scan: %w", err)
		}
		targets = append(targets, t)
	}
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("RideRequestRepo.ListActiveRidePollTargets: rows: %w", err)
	}
	return targets, nil
}
