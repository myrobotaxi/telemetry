// SQL for the per-vehicle ride-window conflict gate (MYR-383) — "a vehicle
// cannot promise two rides in one window". Split from ride_request_queries.go
// so the ONE predicate every landing site shares sits with the two statements
// that carry it and the lock that makes them safe.

package store

// rideWindowConflictPredicate is the SINGLE definition of "this vehicle is
// already promised to somebody at this instant" (MYR-383), written against an
// aliased `r` so both landing sites — the reservation INSERT and the guarded
// requested->accepted UPDATE — share it character-for-character. It is the
// MYR-383 analogue of activeInstantRidePredicate (queries.go), and for the same
// reason: two readers of the same rule drift, one const cannot.
//
// THE MODEL, in one sentence: every OPEN ride on a vehicle OCCUPIES a window
// of ±$4 seconds around its ride instant — `scheduled_for` for a reservation,
// `NOW()` for an active instant ride — and a reservation for $2 is refused when
// its instant falls INSIDE any of them.
//
// Two arms, because a ride's instant has two spellings:
//
//   - RESERVATION arm. A reservation is a promise for a named future instant,
//     so it occupies a window around `scheduled_for`. COMMITTED reservations
//     (`accepted`/`arrived`/`enroute`) always occupy it. A merely `requested`
//     one occupies it only when $5 is true — see below.
//   - ACTIVE INSTANT arm. An instant ride has no `scheduled_for`; its instant
//     is NOW, so it occupies a window around `NOW()`. The status set is the
//     COMMITTED one — character for character activeInstantRidePredicate's, and
//     for its reason: a merely `requested` instant ride has not been promised
//     to anybody yet, so it cannot promise the car away. A car mid-ride cannot
//     also collect somebody 20 minutes out.
//
// $5 — WHY THE TWO LANDING SITES DISAGREE ABOUT `requested`, in ONE predicate.
// A pending reservation is a CLAIM on a slot but not a COMMITMENT of the car,
// and the two callers are asking different questions of it:
//
//   - CREATE passes TRUE. "Is this slot already claimed?" A rider must not be
//     handed a 3:20 PM booking that is going to collide with somebody's pending
//     3:00 PM request, because resolving it costs the owner a hand-decline —
//     the treadmill this whole surface keeps trying to end. This is the arm
//     that closes the reported defect.
//   - ACCEPT passes FALSE. "Is the car already COMMITTED at this hour?" An
//     owner's accept is precisely HOW a contested slot is decided, so counting
//     the peer request would refuse BOTH sides of a legacy double-booking and
//     leave the owner unable to confirm either — the exact stranding the gate
//     exists to prevent, arrived at from the other side. It would also make the
//     refusal say a slot is "booked" when nobody has booked it. Peer requests
//     lose the moment one of them is accepted: the winner is COMMITTED, and the
//     loser's accept is then refused truthfully by the arm above.
//
// The two questions cannot collapse into one, but the RULE does: same
// predicate, same window, same constant, one flag. Under the per-vehicle
// booking lock the accept case still admits exactly one winner — the loser's
// probe runs after the winner has COMMITTED and sees it.
//
// Terminal rows (`completed`/`declined`/`cancelled`) fall out of every arm, so
// a declined reservation frees its window immediately — the refusal is a
// DEFERRAL, never a permanent hold on a slot.
//
// The comparisons are STRICTLY inside the window on both sides, so two rides
// exactly $4 seconds apart are ALLOWED (see RideConflictWindow). A new
// committed lifecycle state must be added to BOTH arms here in the same PR that
// introduces it.
//
// MYR-385 FACTORED IT, DID NOT COPY IT. The picker read surface
// (queryVehicleBookedWindows) has to answer the INVERSE question — "which
// intervals would this predicate refuse?" — and a second spelling of the rule
// would drift from this one within a release. So the rule is assembled from the
// fragments below and the read surface is built from the SAME ones: per-arm
// occupancy (who counts) and rideWindowOverlap (how far).
//
// The two arms carry their OWN range conjuncts rather than sharing one
// COALESCE'd pair. That is not an oversight — see rideWindowOverlap; it is the
// only spelling Postgres can match to idx_go_ride_requests_vehicle_window.
var rideWindowConflictPredicate = `(
			` + rideWindowReservationOccupies + `
			AND ` + rideWindowOverlap(`r.scheduled_for`, `$2`, `$2`) + `
		) OR (
			` + rideWindowInstantOccupies + `
			AND ` + rideWindowOverlap(`NOW()`, `$2`, `$2`) + `
		)`

// rideWindowCommittedStatuses is the COMMITTED lifecycle set — the statuses in
// which a car has actually been promised to somebody, as opposed to merely
// asked for. Character for character activeInstantRidePredicate's set
// (queries.go) and the reason is the same one that motivates every const in
// this file: a lifecycle state added to one list and not the other is a rule
// that disagrees with itself. Terminal rows (completed/declined/cancelled) are
// absent from it, which is what makes a refusal a DEFERRAL — decline the holder
// and the window frees immediately.
const rideWindowCommittedStatuses = `('accepted', 'arrived', 'enroute')`

// rideWindowReservationOccupies / rideWindowInstantOccupies are "ride r
// OCCUPIES a window at all", per arm — the membership half of the rule, with
// the geometry factored out. Two fragments rather than one OR'd pair because
// the two arms anchor on different columns, so each has to sit next to its own
// range conjuncts (see rideWindowOverlap).
//
// A reservation counts on its `scheduled_for`, always when committed and
// additionally when merely `requested` if $5 says pending claims count; an
// instant ride counts only when COMMITTED, because a `requested` instant ride
// has not been promised to anybody yet and cannot promise the car away.
//
// POSITIONAL CONTRACT: the reservation fragment binds $5 (count-pending). Every
// statement that embeds it MUST put count-pending at $5 —
// queryRideWindowConflict and queryVehicleBookedWindows both do. A mismatch is
// a type error at the first execution rather than a wrong answer, but keep the
// positions aligned anyway.
const rideWindowReservationOccupies = `r.scheduled_for IS NOT NULL
			AND (r.status IN ` + rideWindowCommittedStatuses + `
			     OR (r.status = 'requested' AND $5))`

const rideWindowInstantOccupies = `r.scheduled_for IS NULL
			AND r.status IN ` + rideWindowCommittedStatuses

// rideWindowHalfWidthExpr is W — the half-width bound at $4, in seconds. Every
// endpoint in this package is spelled through it, so RideConflictWindow reaches
// the gate and the MYR-385 read surface through ONE bind and cannot be widened
// for one and not the other.
//
// POSITIONAL CONTRACT: it binds $4.
const rideWindowHalfWidthExpr = `make_interval(secs => $4::float8)`

// rideWindowOverlap spells "the window ride r occupies overlaps [lower, upper]"
// for ONE arm, against that arm's own anchor — `r.scheduled_for` for the
// reservation arm, `NOW()` for the active-instant arm. The gate passes the same
// placeholder for both bounds (a single proposed instant); the read passes the
// caller's range.
//
// IT LOOKS REDUNDANT AND IS NOT. DO NOT "SIMPLIFY" IT.
//
// The natural spelling is the one this expression is algebraically equal to:
//
//	anchor - W < upper  AND  anchor + W > lower          -- window vs bounds
//	anchor < upper + W  AND  anchor > lower - W          -- W moved across
//
// The second line is what this function emits, and the ONLY difference that
// matters is which side W sits on. With W on the anchor's side the anchor is an
// EXPRESSION, and Postgres cannot match an expression to a plain btree column:
// the range stops being an index condition, and with it the only reason to use
// idx_go_ride_requests_vehicle_window (vehicle_id, scheduled_for) at all — the
// planner abandons the index and reads the table.
//
// EXPLAIN-VERIFIED, on a car with 1241 open reservations among 11169 rows
// (ride_request_window_index_plan_test.go seeds exactly that):
//
//	W on the anchor:  Seq Scan, Rows Removed by Filter 11168, 251 buffers
//	W on the bounds:  Bitmap Index Scan, Index Cond carries both scheduled_for
//	                  bounds, 1 row, Heap Blocks 1, 9 buffers
//
// That is ~16x on the conflict probe, which runs on every create and every
// accept INSIDE the per-vehicle advisory booking lock, and ~10x on the picker
// read, which would otherwise scan a whole calendar to answer about two days of
// it. With W on the CONSTANT side the anchor stands bare, the conjuncts are
// SARGable, and the range rides the index.
//
// The inequalities are STRICT on both sides, which is the property the read
// surface most needs to inherit from the gate: the endpoints are EXCLUSIVE, so
// two rides exactly W apart are ALLOWED, and a picker that dimmed the endpoints
// would refuse a slot the server would have taken.
//
// `lower` happens to be $2 at both call sites today; it stays an explicit
// parameter (//nolint:unparam) because the two bounds are what DISTINGUISHES the
// gate from the read — the gate passes one instant as both, the read passes a
// range — and collapsing the one that currently agrees would hide that.
//
//nolint:unparam // see above: naming both bounds is the point of the helper.
func rideWindowOverlap(anchor, lower, upper string) string {
	return anchor + ` > ` + lower + `::timestamptz - ` + rideWindowHalfWidthExpr + `
			AND ` + anchor + ` < ` + upper + `::timestamptz + ` + rideWindowHalfWidthExpr
}

// rideWindowAnchorExpr is the INSTANT a ride occupies its window around:
// `scheduled_for` for a reservation, NOW() for an instant ride, which has no
// scheduled instant because it is happening now.
//
// The COALESCE is exact, not an approximation: every statement that uses it has
// already split the arms on `scheduled_for IS NULL`, so it resolves to precisely
// the column the matching arm compared against. It is used ONLY where it costs
// nothing — the gate's ORDER BY and the read's SELECT projection, neither of
// which an index could serve anyway. It must NOT appear in a WHERE clause; see
// rideWindowOverlap for why.
const rideWindowAnchorExpr = `COALESCE(r.scheduled_for, NOW())`

// rideWindowStartExpr / rideWindowEndExpr are the OPEN interval ride r occupies,
// as a SELECTABLE pair of instants — the anchor plus or minus the half-width.
// They exist for the MYR-385 read surface's projection, which has to hand the
// picker two concrete instants; the gate never needs them because it compares
// rather than emits. Both are PROJECTION-ONLY for the reason on
// rideWindowOverlap.
//
// POSITIONAL CONTRACT: both bind $4.
const rideWindowStartExpr = rideWindowAnchorExpr + ` - ` + rideWindowHalfWidthExpr
const rideWindowEndExpr = rideWindowAnchorExpr + ` + ` + rideWindowHalfWidthExpr

// queryRideWindowConflict finds the conflicting ride NEAREST to the proposed
// instant, if any. $1 vehicle_id, $2 the proposed instant, $3 the ride id to
// EXCLUDE (the row being accepted; the empty string on create, which matches no
// id), $4 the window half-width in seconds, $5 whether PENDING claims count.
//
// It returns exactly two facts about the conflict: WHEN the car is spoken for
// (NULL for the active-instant arm — that ride is happening now and has no
// scheduled instant), and whether that claim is merely PENDING, which decides
// only which sentence the caller says. Nothing else leaves this statement: the
// conflicting ride's id, rider, requester name, pickup and dropoff are another
// party's business (P1, data-classification.md §1.9). The instant and the
// pending flag are P0 operational timing — the same tier as `status` — so
// echoing them in an error message is safe.
//
// NEAREST-first ordering (not newest, not soonest) so the instant named in the
// refusal is the one the rider would actually collide with; `id` breaks ties
// deterministically. LIMIT 1 because one conflict is a refusal — enumerating
// the rest would leak the shape of somebody else's calendar.
//
// The index that serves it is idx_go_ride_requests_vehicle_window (migration
// 0026), and specifically it serves THREE conjuncts of the reservation arm:
// `r.vehicle_id = $1` (equality, leading column) plus the two `r.scheduled_for`
// range bounds (second column) — the plan carries all three as Index Cond,
// which is why the bare-column spelling in rideWindowOverlap is load-bearing.
// The arm's status/NOT NULL conjuncts are the index's own partial predicate, so
// they are proved rather than probed. The ACTIVE-INSTANT arm is a second bitmap
// branch over the `scheduled_for IS NULL` partial indexes from 0004/0013; its
// NOW() bounds are not index conditions and do not need to be — that arm holds
// at most one row per vehicle.
var queryRideWindowConflict = `SELECT r.scheduled_for, r.status = 'requested'
FROM go_ride_requests r
WHERE r.vehicle_id = $1
  AND r.id <> $3
  AND (` + rideWindowConflictPredicate + `)
ORDER BY ABS(EXTRACT(EPOCH FROM (` + rideWindowAnchorExpr + ` - $2::timestamptz))) ASC, r.id ASC
LIMIT 1`

// rideVehicleLockNamespace is the advisory-lock class id for the MYR-383
// per-vehicle booking lock. Postgres advisory locks live in ONE process-wide
// (int4, int4) space shared by every extension and application on the database,
// so the first key namespaces this server's locks away from anybody else's; the
// second is the vehicle. The value is the Linear issue number — arbitrary, but
// traceable to the reason the lock exists.
const rideVehicleLockNamespace = 383

// queryRideVehicleLock takes the per-vehicle booking lock for the REST of the
// enclosing transaction, keyed by vehicle id. `pg_advisory_xact_lock` is
// released by COMMIT or ROLLBACK with no unlock call and no leak on a panicking
// or disconnecting session, which is the whole reason it is the xact variant.
//
// hashtext() collapses the cuid into the int4 the lock space wants. A HASH
// COLLISION between two vehicle ids is harmless by construction: it can only
// make two unrelated cars serialize with each other for the duration of one
// booking, never let two bookings for the SAME car proceed concurrently.
const queryRideVehicleLock = `SELECT pg_advisory_xact_lock($1::int4, hashtext($2::text))`

// queryRideVehicleLockByRide is queryRideVehicleLock keyed by RIDE id instead:
// it locks whichever vehicle the ride targets, without a round trip to learn
// which that is. Zero rows (an unknown/deleted ride) takes no lock and is not
// an error — the guarded UPDATE that follows is what reports a missing row.
const queryRideVehicleLockByRide = `SELECT pg_advisory_xact_lock($1::int4, hashtext(vehicle_id))
FROM go_ride_requests
WHERE id = $2`

// queryRideRequestBooking reads the two facts the accept-side gate needs about
// the ride being accepted: which vehicle it is promising, and for when. Run
// INSIDE the lock, so the instant it checks against is the current one even if
// a reschedule moved it a moment ago.
const queryRideRequestBooking = `SELECT vehicle_id, scheduled_for
FROM go_ride_requests
WHERE id = $1`
