// Lifecycle-transition SQL for go_ride_requests. Split out of
// ride_request_queries.go (MYR-376) so the three transition statements sit
// beside each other and share ONE stamping clause: they differ only in their
// WHERE, and a timestamp side-effect that drifted between them would be a
// silent data bug.

package store

// rideRequestStatusStamp is the SET clause every lifecycle transition shares.
// One statement, all of the timestamp side-effects: entering 'accepted' stamps
// accepted_at, entering 'arrived' stamps picked_up_at (the owner-confirmed
// pickup instant — MYR-270), entering 'enroute' stamps enroute_at (the
// rider-started leg-2 instant), entering 'completed' stamps completed_at (each
// only on FIRST entry — a re-update never moves an already-set stamp), and
// every transition touches updated_at.
//
// $1 = ride id, $2 = target status. Callers append their own WHERE + RETURNING.
const rideRequestStatusStamp = `UPDATE go_ride_requests SET
	status = $2,
	accepted_at = CASE
		WHEN $2 = 'accepted' AND accepted_at IS NULL THEN NOW()
		ELSE accepted_at
	END,
	enroute_at = CASE
		WHEN $2 = 'enroute' AND enroute_at IS NULL THEN NOW()
		ELSE enroute_at
	END,
	picked_up_at = CASE
		WHEN $2 = 'arrived' AND picked_up_at IS NULL THEN NOW()
		ELSE picked_up_at
	END,
	completed_at = CASE
		WHEN $2 = 'completed' AND completed_at IS NULL THEN NOW()
		ELSE completed_at
	END,
	updated_at = NOW()`

// queryRideRequestUpdateStatus is the UNGUARDED transition: it stamps whatever
// the row's current status is. Transition legality is the caller's concern.
const queryRideRequestUpdateStatus = rideRequestStatusStamp + `
WHERE id = $1
RETURNING ` + rideRequestColumns

// queryRideRequestUpdateStatusFrom is the GUARDED transition variant
// (MYR-174/175 check-then-write race fix): identical timestamp-stamping
// semantics to queryRideRequestUpdateStatus, but the row only matches when
// its CURRENT status is in the caller's allowed-from set ($3, text[]).
// Concurrent conflicting transitions therefore serialize in the database —
// exactly one UPDATE matches; the loser affects zero rows and the repo maps
// that to ErrRideRequestConflict (or ErrRideRequestNotFound when the id is
// unknown). This guard is what makes the ride.accepted dispatch event
// exactly-once per accept: only the winning write's caller publishes.
const queryRideRequestUpdateStatusFrom = rideRequestStatusStamp + `
WHERE id = $1 AND status = ANY($3)
RETURNING ` + rideRequestColumns

// queryRideRequestUpdateStatusFromDispatched is the guarded transition PLUS the
// MYR-376 reservation-dormancy predicate. It backs the owner pickup transition
// (accepted → arrived) and nothing else.
//
// THE DEFECT IT CLOSES. A reservation is DORMANT between accept and dispatch:
// the MYR-179 sweeper is the only thing that moves it into its live phase (JIT
// claim, leg-1 nav push, dispatch_status='sent', `ride.due`). Before this
// predicate the owner could tap "Picked up" the moment they accepted — a full
// day before `scheduledFor`, with the car still in service and no nav ever
// pushed — stranding the ride at `arrived`, from which the rider's `start`
// would have pushed a real dropoff nav to an in-service vehicle.
//
// THE PREDICATE is `(scheduled_for IS NULL OR dispatch_status = 'sent' OR
// scheduled_for <= NOW())`, and each arm is there for its own reason:
//
//   - `scheduled_for IS NULL` — an INSTANT ride is entirely unaffected. Its
//     pickup must keep working even when its nav push FAILED or was SKIPPED
//     (kill-switch): the car is already at the kerb and the owner drives
//     anyway, so gating an instant pickup on a dispatch outcome would break a
//     working ride to punish a failed notification.
//   - `dispatch_status = 'sent'` — for a reservation, proof that the sweeper
//     woke it up. NULL never compares equal, so an undispatched reservation
//     (the production case) does not match before its due instant.
//   - `scheduled_for <= NOW()` — DORMANCY ENDS AT THE DUE INSTANT, not at a
//     successful dispatch. §7.8's reservation-expiry contract promises that a
//     ride the sweeper FAILED (`reservation_expired`, dispatch 'failed' — or
//     'skipped', or a sweeper that never ran) stays `accepted` and "the
//     owner/rider may still cancel or proceed manually"; a predicate demanding
//     'sent' would strand every such ride at `accepted` with cancel as its only
//     exit. Past due, a manual pickup is the documented recovery path. The
//     defect this gate closes is strictly the PRE-DUE pickup — "picked up" a
//     day before the ride. For a fixed `scheduled_for` the arm is MONOTONE
//     (the clock only advances), so dormancy never returns on its own; the one
//     thing that can re-impose it is a CONFIRMED RESCHEDULE moving the instant
//     later, which is correct — the ride is genuinely due later now.
//
// It does NOT gate on whether the sweeper's leg-1 nav actually reached the car,
// and must not: at/after due the owner is standing at the vehicle, and the whole
// point of the manual-proceed contract is that the handshake survives a dispatch
// that never landed.
//
// It is evaluated INSIDE the same UPDATE as the status guard, so a sweeper
// dispatching concurrently with a pickup tap is arbitrated by the database
// exactly like two racing transitions: no pickup can ever commit against a row
// that is dormant at the instant of the write, whatever any earlier read saw.
const queryRideRequestUpdateStatusFromDispatched = rideRequestStatusStamp + `
WHERE id = $1 AND status = ANY($3)
  AND (scheduled_for IS NULL OR dispatch_status = 'sent' OR scheduled_for <= NOW())
RETURNING ` + rideRequestColumns
