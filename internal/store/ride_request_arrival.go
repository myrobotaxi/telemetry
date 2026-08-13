// The LEAN arrival-candidate read (MYR-538).
//
// The auto-arrival detector watches decoded telemetry frames — one per second
// per streaming car — and needs one thing from the database: "which cars are
// currently driving to a pickup, and where is that pickup". It CANNOT ask per
// frame, so it asks on a ~15s cache and matches frames against the answer in
// memory. That makes this read the same shape as the MYR-535 vehicle-position
// read and the MYR-394 poll-target read: a background loop's periodic probe,
// which must never drag a wide projection behind it.
//
// Two ciphertext columns and two decrypts per row — the PICKUP only, exactly
// like scanDueReservation. The dropoff, the passenger fields and the requester
// identity subselects are all outside the projection: a loop that only measures
// distance has no business decrypting or holding them.
//
// The "Vehicle" join is a READ (CG-DL-9 constrains writes). It is here because
// telemetry frames are keyed by VIN and rides are keyed by vehicle cuid, and
// resolving that per frame is precisely the per-frame query this design exists
// to avoid.

package store

import (
	"context"
	"fmt"
	"time"
)

// defaultArrivalCandidateLimit caps one refresh when the caller passes
// limit <= 0. Candidates are cars mid-leg-1 across the whole fleet at one
// instant, so the ceiling is generous by construction; the cap exists so a
// pathological table can never turn a cache refresh into an unbounded scan.
const defaultArrivalCandidateLimit = 100

// ArrivalCandidate is one ride whose car is on its way to a WAYPOINT of that
// ride: the ids the events need, the VIN telemetry frames are keyed by, and the
// decrypted coordinate of the place the distance test measures against.
//
// Since MYR-539 the waypoint is not always the pickup. A ride under way
// (`enroute`) is watched too, against its CURRENT stop — failing that (MYR-550)
// the earliest UPCOMING one — or, when the trip has no stops left ahead of it,
// against the final destination.
//
// Deliberately NOT a RideRequestRecord, for the ListActiveRidePollTargets
// reason plus one more: this projection is refreshed every ~15s for as long as
// any car is mid-pickup, and the wide record would decrypt four coordinates and
// run eight identity subselects per row each time to serve a haversine.
type ArrivalCandidate struct {
	// RideRequestID is the ride the arrival would advance.
	RideRequestID string
	// VehicleID is the ride's vehicle (the Prisma "Vehicle"."id").
	VehicleID string
	// RiderID / OwnerID are the two parties, carried so the published events
	// need no second read.
	RiderID string
	OwnerID string
	// VIN keys the telemetry frame stream. P1 — log redacted.
	VIN string
	// Waypoint names WHICH place this candidate is watching, in the
	// RideWaypointArrivedEvent vocabulary: "pickup", "stop:{id}", or
	// "destination". It is resolved in SQL, in the same statement as the
	// coordinate, so the label and the place it labels can never disagree.
	Waypoint string
	// TargetLatitude / TargetLongitude are the decrypted coordinate of that
	// waypoint. P1 GPS data — never logged.
	TargetLatitude  float64
	TargetLongitude float64
}

// arrivalCandidatePredicate is the single definition of "this car is driving to
// a place this ride cares about RIGHT NOW".
//
// Each conjunct is load-bearing:
//
//	status = 'accepted'          — leg 1, target the PICKUP. `arrived` is
//	  AND dispatch_status='sent'   already picked up (the detector's own
//	                               outcome, or the owner's tap), and the
//	                               dispatch conjunct is what keeps a
//	                               reservation booked for next Tuesday out of
//	                               the set: its car may well be parked at the
//	                               rider's address today, and position alone
//	                               would mark the ride picked up.
//	status = 'enroute'           — leg 2 (MYR-539), target the ride's CURRENT
//	                               stop or, with none, the destination. No
//	                               dispatch conjunct: leg 2's own latch lives in
//	                               different columns, and a rider who started
//	                               the ride is aboard whether or not the nav
//	                               push landed — which is exactly the case
//	                               (MYR-527) where the dash cannot be trusted
//	                               and the car's POSITION is all there is.
//	updated_at > NOW() - 6 hours — the MYR-394 age bound, for the identical
//	                               reason: no status ever expires on its own, so
//	                               an abandoned ride would otherwise sit in this
//	                               set for the life of the database. Written
//	                               against the bare column so it stays SARGable
//	                               against idx_go_ride_requests_active_poll.
//
// It remains strictly NARROWER than pollableRidePredicate, whose partial index
// (migration 0028, predicate `status IN ('accepted','enroute','arrived')`, key
// updated_at) therefore still serves this query unchanged — both status arms
// imply the index predicate, the age bound is the Index Cond range, and the
// ORDER BY is a backwards ordered scan. No new index, no migration.
const arrivalCandidatePredicate = `(
			   (r.status = 'accepted' AND r.dispatch_status = 'sent')
			OR  r.status = 'enroute'
		  )
		  AND r.updated_at > NOW() - INTERVAL '6 hours'`

// queryArrivalCandidates lists the (ride, vehicle, VIN, target, waypoint) rows
// the arrival detector matches telemetry frames against.
//
// The LATERAL picks the ride's CURRENT stop, which is the one place a
// multi-stop ride under way is driving to. The CASE that consumes it is the
// whole waypoint decision, and it is in SQL rather than in Go so that the
// coordinate and the label describing it are chosen by one expression over one
// snapshot of the row — a Go-side choice would read the stop list separately
// and could label a place it did not fetch.
//
// IT FALLS BACK TO THE EARLIEST UPCOMING STOP (MYR-550), which makes this
// expression the exact twin of the handler's currentLegTarget: current stop,
// else earliest upcoming, else the destination. When the "a ride under way has
// exactly one current stop" invariant holds the fallback selects nothing and
// the row is byte-identical to what it was — its whole job is the case where
// the invariant DOESN'T hold, which is reachable and already acknowledged
// elsewhere in this package: StartFirstStop's promotion runs outside the
// Start's own transaction and is allowed to fail without failing the Start
// (a rider aboard must not be stranded for it), and queryRideStopComplete's
// guard admits an 'upcoming' stop for the same reason. Before this fallback a
// lost promotion cost the whole trip: the detector watched the DESTINATION,
// so no stop ever published ride.waypoint_arrived, nothing advanced the leg
// and MYR-542 never flashed. Now the detector watches the place the
// dispatcher actually sent the car to, and the first arrival there completes
// the stop and re-syncs the statuses.
//
// Rows with an empty VIN are excluded exactly as ListActiveRidePollTargets
// excludes them: the MYR-257 provisioning INSERT can seed a placeholder row
// before Tesla has supplied a VIN, and a frame can never be keyed by one.
//
// Ordered most-recently-updated first so a pass truncated by the limit keeps
// the freshest rides — the same inversion MYR-394 argued for, and for the same
// reason: the ride that just transitioned is the one whose rider is watching.
const queryArrivalCandidates = `
SELECT r.id, r.vehicle_id, r.rider_id, r.owner_id, v."vin",
       CASE WHEN r.status = 'accepted' THEN r.pickup_lat_enc
            WHEN s.id IS NOT NULL      THEN s.lat_enc
            ELSE r.dropoff_lat_enc END AS target_lat_enc,
       CASE WHEN r.status = 'accepted' THEN r.pickup_lng_enc
            WHEN s.id IS NOT NULL      THEN s.lng_enc
            ELSE r.dropoff_lng_enc END AS target_lng_enc,
       CASE WHEN r.status = 'accepted' THEN 'pickup'
            WHEN s.id IS NOT NULL      THEN 'stop:' || s.id
            ELSE 'destination' END     AS waypoint
FROM go_ride_requests r
JOIN "Vehicle" v ON v."id" = r.vehicle_id
LEFT JOIN LATERAL (
	SELECT st.id, st.lat_enc, st.lng_enc
	FROM go_ride_stops st
	WHERE st.ride_id = r.id AND st.status IN ('current', 'upcoming')
	ORDER BY (st.status <> 'current'), st.position
	LIMIT 1
) s ON TRUE
WHERE ` + arrivalCandidatePredicate + `
  AND v."vin" <> ''
ORDER BY r.updated_at DESC
LIMIT $1`

// ListArrivalCandidates returns up to limit rides whose car is currently
// driving to one of that ride's waypoints, with the target decrypted.
//
// A non-positive limit uses defaultArrivalCandidateLimit. An empty (non-nil)
// slice is the steady state — most of the time no car in the fleet is mid-
// pickup — and is not an error.
func (r *RideRequestRepo) ListArrivalCandidates(ctx context.Context, limit int) ([]ArrivalCandidate, error) {
	if limit <= 0 {
		limit = defaultArrivalCandidateLimit
	}
	const op = "ride_request.list_arrival_candidates"
	start := time.Now()

	rows, err := r.pool.Query(ctx, queryArrivalCandidates, limit)
	if err != nil {
		r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("RideRequestRepo.ListArrivalCandidates: query: %w", err)
	}
	defer rows.Close()

	out := make([]ArrivalCandidate, 0)
	for rows.Next() {
		var (
			c            ArrivalCandidate
			targetLatEnc string
			targetLngEnc string
		)
		if err := rows.Scan(
			&c.RideRequestID, &c.VehicleID, &c.RiderID, &c.OwnerID, &c.VIN,
			&targetLatEnc, &targetLngEnc, &c.Waypoint,
		); err != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("RideRequestRepo.ListArrivalCandidates: scan: %w", err)
		}
		if c.TargetLatitude, err = r.decryptCoord("target_lat_enc", targetLatEnc); err != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("RideRequestRepo.ListArrivalCandidates: %w", err)
		}
		if c.TargetLongitude, err = r.decryptCoord("target_lng_enc", targetLngEnc); err != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("RideRequestRepo.ListArrivalCandidates: %w", err)
		}
		out = append(out, c)
	}
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("RideRequestRepo.ListArrivalCandidates: rows: %w", err)
	}
	return out, nil
}
