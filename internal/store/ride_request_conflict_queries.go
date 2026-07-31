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
// would drift from this one within a release. So the rule is now assembled from
// three fragments below, and the read surface is built from the SAME three:
// occupancy (who counts), and the two window endpoints (how far). This
// expression is unchanged in meaning — see rideWindowStartExpr for the algebra.
const rideWindowConflictPredicate = rideWindowOccupiedPredicate + `
			AND ` + rideWindowStartExpr + ` < $2::timestamptz
			AND ` + rideWindowEndExpr + ` > $2::timestamptz`

// rideWindowCommittedStatuses is the COMMITTED lifecycle set — the statuses in
// which a car has actually been promised to somebody, as opposed to merely
// asked for. Character for character activeInstantRidePredicate's set
// (queries.go) and the reason is the same one that motivates every const in
// this file: a lifecycle state added to one list and not the other is a rule
// that disagrees with itself. Terminal rows (completed/declined/cancelled) are
// absent from it, which is what makes a refusal a DEFERRAL — decline the holder
// and the window frees immediately.
const rideWindowCommittedStatuses = `('accepted', 'arrived', 'enroute')`

// rideWindowOccupiedPredicate is "ride r OCCUPIES a window at all" — the
// membership half of the rule, with the geometry factored out. Two arms,
// because a ride's instant has two spellings (see the narrative above):
// a reservation counts on its `scheduled_for`, always when committed and
// additionally when merely `requested` if $5 says pending claims count; an
// instant ride counts only when COMMITTED, because a `requested` instant ride
// has not been promised to anybody yet and cannot promise the car away.
//
// POSITIONAL CONTRACT: this fragment binds $5 (count-pending). Every statement
// that embeds it MUST put count-pending at $5 — queryRideWindowConflict and
// queryVehicleBookedWindows both do. A mismatch is a type error at the first
// execution rather than a wrong answer, but keep the positions aligned anyway.
const rideWindowOccupiedPredicate = `(
			(r.scheduled_for IS NOT NULL
			 AND (r.status IN ` + rideWindowCommittedStatuses + `
			      OR (r.status = 'requested' AND $5)))
			OR (r.scheduled_for IS NULL
			    AND r.status IN ` + rideWindowCommittedStatuses + `)
		)`

// rideWindowAnchorExpr is the INSTANT a ride occupies its window around:
// `scheduled_for` for a reservation, NOW() for an instant ride, which has no
// scheduled instant because it is happening now.
//
// The COALESCE is what let the two arms' geometry collapse into one expression.
// It is exact, not an approximation: rideWindowOccupiedPredicate has already
// split the arms on `scheduled_for IS NULL`, so within either arm the COALESCE
// resolves to precisely the column that arm's original spelling used.
const rideWindowAnchorExpr = `COALESCE(r.scheduled_for, NOW())`

// rideWindowStartExpr / rideWindowEndExpr are the OPEN interval ride r occupies
// — the anchor plus or minus the half-width bound at $4. They are the ONE
// definition of the window's geometry, and the reason both the gate and the
// MYR-385 read surface can be trusted to describe the same intervals.
//
// THE ALGEBRA, so the factoring can be checked rather than believed. The gate
// originally asked `anchor > $2 - W AND anchor < $2 + W`; it now asks
// `anchor - W < $2 AND anchor + W > $2`. Those are the same two inequalities
// with W moved across, so the gate's behaviour is byte-identical — including
// its STRICTNESS, which is the property the read surface most needs to inherit:
// the endpoints are EXCLUSIVE, so a booking at exactly `start` or exactly `end`
// is ALLOWED, and a picker that dims the endpoints would refuse a slot the
// server would have taken.
//
// POSITIONAL CONTRACT: both bind $4 (the half-width in seconds).
const rideWindowStartExpr = rideWindowAnchorExpr + ` - make_interval(secs => $4::float8)`
const rideWindowEndExpr = rideWindowAnchorExpr + ` + make_interval(secs => $4::float8)`

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
// 0026) for the reservation arm and uq_go_ride_requests_active_instant_vehicle
// (0013) for the instant arm.
const queryRideWindowConflict = `SELECT r.scheduled_for, r.status = 'requested'
FROM go_ride_requests r
WHERE r.vehicle_id = $1
  AND r.id <> $3
  AND (` + rideWindowConflictPredicate + `)
ORDER BY ABS(EXTRACT(EPOCH FROM (COALESCE(r.scheduled_for, NOW()) - $2::timestamptz))) ASC, r.id ASC
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
