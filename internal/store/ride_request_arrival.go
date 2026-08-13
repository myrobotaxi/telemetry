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

// ArrivalCandidate is one ride whose car is on its way to the pickup: the ids
// the status event needs, the VIN telemetry frames are keyed by, and the
// decrypted pickup coordinate the distance test measures against.
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
	// PickupLatitude / PickupLongitude are the decrypted pickup coordinate.
	// P1 GPS data — never logged.
	PickupLatitude  float64
	PickupLongitude float64
}

// arrivalCandidatePredicate is the single definition of "this car is driving to
// a pickup RIGHT NOW".
//
// Each conjunct is load-bearing:
//
//	status = 'accepted'          — leg 1. `arrived` is already picked up (the
//	                               detector's own outcome, or the owner's tap);
//	                               `enroute` is leg 2, whose endpoint is the
//	                               DROP-OFF and which v1 does not detect;
//	                               everything else is terminal or unaccepted.
//	dispatch_status = 'sent'     — the car has actually been TOLD to go. This is
//	                               the same "the reservation is live" test the
//	                               MYR-394 poll predicate uses, and it is what
//	                               keeps a reservation booked for next Tuesday
//	                               out of the candidate set: its car may well be
//	                               parked at the rider's address today, and
//	                               without this conjunct that alone would mark
//	                               the ride picked up. It also holds for instant
//	                               rides, whose leg-1 push resolves seconds
//	                               after the accept.
//	updated_at > NOW() - 6 hours — the MYR-394 age bound, for the identical
//	                               reason: no status ever expires on its own, so
//	                               an abandoned ride would otherwise sit in this
//	                               set for the life of the database. Written
//	                               against the bare column so it stays SARGable
//	                               against idx_go_ride_requests_active_poll.
//
// It is strictly NARROWER than pollableRidePredicate, whose partial index
// (migration 0028, predicate `status IN ('accepted','enroute','arrived')`,
// key updated_at) therefore serves this query unchanged — status = 'accepted'
// implies the index predicate, the age bound is the Index Cond range, and the
// ORDER BY is a backwards ordered scan. No new index, no migration.
const arrivalCandidatePredicate = `r.status = 'accepted'
		  AND r.dispatch_status = 'sent'
		  AND r.updated_at > NOW() - INTERVAL '6 hours'`

// queryArrivalCandidates lists the (ride, vehicle, VIN, pickup) rows the
// arrival detector matches telemetry frames against.
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
       r.pickup_lat_enc, r.pickup_lng_enc
FROM go_ride_requests r
JOIN "Vehicle" v ON v."id" = r.vehicle_id
WHERE ` + arrivalCandidatePredicate + `
  AND v."vin" <> ''
ORDER BY r.updated_at DESC
LIMIT $1`

// ListArrivalCandidates returns up to limit rides whose car is currently
// driving to its pickup, with the pickup decrypted.
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
			pickupLatEnc string
			pickupLngEnc string
		)
		if err := rows.Scan(
			&c.RideRequestID, &c.VehicleID, &c.RiderID, &c.OwnerID, &c.VIN,
			&pickupLatEnc, &pickupLngEnc,
		); err != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("RideRequestRepo.ListArrivalCandidates: scan: %w", err)
		}
		if c.PickupLatitude, err = r.decryptCoord("pickup_lat_enc", pickupLatEnc); err != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("RideRequestRepo.ListArrivalCandidates: %w", err)
		}
		if c.PickupLongitude, err = r.decryptCoord("pickup_lng_enc", pickupLngEnc); err != nil {
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
