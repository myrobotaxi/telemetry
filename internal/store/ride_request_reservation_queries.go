// SQL for the MYR-179 reservation-time dispatch sweep. Split from
// ride_request_queries.go (already at the 300-line file cap) so the
// scheduled-dispatch read paths stay together and reviewable.
//
// v1 changes only WHEN the leg-1 (pickup) nav push fires for a SCHEDULED
// ride: the accept no longer dispatches it, and a periodic sweeper fires it
// at `scheduled_for`. It reuses the EXISTING MYR-176 latch columns
// (dispatched_at / dispatch_status / dispatch_error) unchanged — no new
// column and no new status value. Migration 0016 adds ONE partial index (no
// schema change) so the 30s sweep stays an index probe forever.

package store

// rideRequestDueColumns is the LEAN sweep projection, aliased to the outer
// `d` (due) relation. Deliberately NOT rideRequestColumns: the sweeper needs
// only what it takes to decide and push — the ids, the PICKUP place, and
// `scheduled_for` for the lateness deadline. The wide projection would drag
// requesterIdentitySelect's eight correlated identity subselects and two extra
// coordinate decrypts (dropoff) per row, re-paid every 30s tick for every row
// the pass merely holds, for data the sweeper then throws away.
//
// Passenger name/phone (P1) and the requester identity (P1 PII) are outside
// this projection entirely: a background dispatch loop has no business
// decrypting or holding them.
const rideRequestDueColumns = `d.id, d.rider_id, d.owner_id, d.vehicle_id,
	d.pickup_lat_enc, d.pickup_lng_enc, d.pickup_label, d.pickup_address,
	d.scheduled_for`

// queryRideRequestListDue selects reservations whose pickup nav push is DUE.
// The static conjuncts are the whole contract of scheduled dispatch:
//
//	scheduled_for IS NOT NULL  — a reservation, not an instant ride (an
//	                             instant ride still dispatches on accept).
//	status = 'accepted'        — the owner committed to it AND it has not
//	                             moved on. A `requested` reservation was never
//	                             accepted; `cancelled`/`declined` must never be
//	                             dialed; `arrived`/`enroute`/`completed` are
//	                             already past the pickup push. Re-checked AT
//	                             CLAIM TIME as well (see
//	                             queryRideRequestClaimReservationDispatch) —
//	                             this filter alone is only as fresh as the pass.
//	dispatched_at IS NULL      — the leg-1 latch is UNCLAIMED. This is what
//	                             makes the sweep self-terminating: a claimed
//	                             row (won by a peer sweeper, or resolved to
//	                             sent/failed/skipped) can never be re-selected,
//	                             so a ride is dispatched at most once no matter
//	                             how many sweepers or ticks run.
//	scheduled_for <= $1        — the reservation is inside the DISPATCH
//	                             HORIZON. $1 is `now + LeadCap` (MYR-535), not
//	                             the sweeper's bare clock: `scheduled_for` is
//	                             the instant the car should ARRIVE at the
//	                             pickup, so candidates are selected up to the
//	                             maximum dispatch lead early and the per-row
//	                             leave-time is decided in Go — the pickup and
//	                             vehicle coordinates the lead needs are
//	                             encrypted at rest, so it cannot be computed
//	                             here. Both bind instants derive from the
//	                             SWEEPER's clock (never NOW()) so one clock
//	                             governs this selection, the Go-side lead gate
//	                             and the lateness deadline — no app/DB skew can
//	                             put them at odds — and the boundary is exactly
//	                             testable.
//
// Those four are exactly the predicate + leading column of
// idx_go_ride_requests_reservation_due (migration 0016).
//
// The fifth conjunct is the ANTI-STARVATION clause. Held rows (vehicle mid-ride
// inside the lateness window) are deliberately left unclaimed and re-listed
// every tick; with a plain oldest-first LIMIT they re-occupy the head of the
// window forever and a younger due reservation for an IDLE car at position
// LIMIT+1 is never even selected. So a row is selected only when EITHER:
//
//	d.scheduled_for <= $2  — it is past its lateness deadline ($2 = now -
//	                         MaxLateness) and must be RESOLVED this pass
//	                         (claimed and failed `reservation_expired`)
//	                         regardless of what the car is doing; excluding it
//	                         would resurrect the silently-pending row the
//	                         expiry path exists to eliminate.
//	NOT EXISTS (...)       — or its vehicle is free right now, i.e. the row is
//	                         actually dispatchable.
//
// A held row is therefore neither returned nor forgotten: it drops out of the
// LIMIT window while its car is busy and reappears the moment the car frees up
// OR its deadline passes. The alternative (widen/window the LIMIT and filter in
// Go) was rejected: it keeps paying per-row busy probes and a full row
// projection for rows that cannot be acted on, and it caps the pass on rows
// examined rather than on work done.
//
// The busy subquery shares activeInstantRidePredicate with the `hasActiveRide`
// catalog flag and with queryRideRequestVehicleBusy — character-for-character
// the predicate of `uq_go_ride_requests_active_instant_vehicle` (migration
// 0013), which answers it as an index probe. It is a WINDOWING filter only: the
// authoritative busy check runs in Go immediately before the claim, because
// this one is as stale as the pass (see internal/dispatch).
//
// Ordered oldest-reservation-first so a backlog (e.g. after downtime) drains
// in the order riders booked, with id as the deterministic tie-break. $3 caps
// one sweep.
const queryRideRequestListDue = `SELECT ` + rideRequestDueColumns + `
FROM go_ride_requests d
WHERE d.scheduled_for IS NOT NULL
  AND d.status = 'accepted'
  AND d.dispatched_at IS NULL
  AND d.scheduled_for <= $1
  AND (
        d.scheduled_for <= $2
     OR NOT EXISTS (
          SELECT 1
          FROM go_ride_requests r
          WHERE r.vehicle_id = d.vehicle_id
            AND ` + activeInstantRidePredicate + `
        )
      )
ORDER BY d.scheduled_for ASC, d.id ASC
LIMIT $3`

// queryRideRequestClaimReservationDispatch is the reservation-side leg-1
// latch: the SAME dispatched_at column queryRideRequestClaimDispatch stamps
// (so exactly-once holds across both paths and a row claimed by either is
// invisible to the other), plus two guards the instant path does not need.
//
//	status = 'accepted'       — the ride must STILL be accepted at claim time.
//	                            The sweep's own status filter is only as fresh
//	                            as the pass; a rider cancel (accepted →
//	                            cancelled is legal) or an owner picked-up
//	                            (accepted → arrived) landing between the SELECT
//	                            and the claim MUST lose. Without this conjunct
//	                            the claim ignores status entirely and the car
//	                            is dialed to a pickup nobody wants — or
//	                            re-pointed at the PICKUP after the rider is
//	                            already aboard and the dropoff was pushed.
//	scheduled_for IS NOT NULL — the sweeper only ever claims reservations. A
//	                            projection bug can never make it latch an
//	                            instant ride out from under the accept path.
//
// No-row means "someone else got there first, or the ride moved on" — an
// ordinary outcome the sweeper skips SILENTLY, never an error. Kept separate
// from queryRideRequestClaimDispatch on purpose: the instant path's claim runs
// milliseconds after the winning accept write and its behaviour is deliberately
// left byte-identical to MYR-176.
const queryRideRequestClaimReservationDispatch = `UPDATE go_ride_requests SET
	dispatched_at = NOW(),
	updated_at = NOW()
WHERE id = $1
  AND dispatched_at IS NULL
  AND status = 'accepted'
  AND scheduled_for IS NOT NULL
RETURNING id`

// queryRideRequestVehicleBusy reports whether a vehicle is currently
// committed to an active INSTANT ride — the reservation sweeper's
// vehicle-busy hold (MYR-179). It shares activeInstantRidePredicate with the
// `hasActiveRide` catalog flag, which is character-for-character the
// predicate of `uq_go_ride_requests_active_instant_vehicle` (migration 0013),
// so "the sweeper thinks the car is free" and "the accept guard would let an
// instant ride onto this car" can never disagree.
//
// Postgres answers it from that partial index (the same index-only probe the
// catalog flag measured), so it costs one probe per reservation about to be
// claimed.
const queryRideRequestVehicleBusy = `SELECT EXISTS (
	SELECT 1
	FROM go_ride_requests r
	WHERE r.vehicle_id = $1
	  AND ` + activeInstantRidePredicate + `
)`
