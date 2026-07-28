// SQL for the MYR-179 reservation-time dispatch sweep. Split from
// ride_request_queries.go (already at the 300-line file cap) so the
// scheduled-dispatch read paths stay together and reviewable.
//
// v1 changes only WHEN the leg-1 (pickup) nav push fires for a SCHEDULED
// ride: the accept no longer dispatches it, and a periodic sweeper fires it
// at `scheduled_for`. It reuses the EXISTING MYR-176 latch columns
// (dispatched_at / dispatch_status / dispatch_error) unchanged — no new
// column, no new status value, no migration.

package store

// queryRideRequestListDue selects reservations whose pickup nav push is DUE.
// The four conjuncts are the whole contract of scheduled dispatch:
//
//	scheduled_for IS NOT NULL  — a reservation, not an instant ride (an
//	                             instant ride still dispatches on accept).
//	status = 'accepted'        — the owner committed to it AND it has not
//	                             moved on. A `requested` reservation was never
//	                             accepted; `cancelled`/`declined` must never be
//	                             dialed; `arrived`/`enroute`/`completed` are
//	                             already past the pickup push.
//	dispatched_at IS NULL      — the leg-1 latch is UNCLAIMED. This is what
//	                             makes the sweep self-terminating: a claimed
//	                             row (won by a peer sweeper, or resolved to
//	                             sent/failed/skipped) can never be re-selected,
//	                             so a ride is dispatched at most once no matter
//	                             how many sweepers or ticks run.
//	scheduled_for <= $1        — the reservation instant has arrived, judged
//	                             against the SWEEPER's clock (passed in, not
//	                             NOW()) so one clock governs both this
//	                             selection and the Go-side busy-hold deadline
//	                             — no app/DB skew can put them at odds — and
//	                             so the due/not-yet-due boundary is exactly
//	                             testable.
//
// Ordered oldest-reservation-first so a backlog (e.g. after downtime) drains
// in the order riders booked, with id as the deterministic tie-break. $2 caps
// one sweep so a large backlog cannot monopolize a tick.
//
// Note the deliberate ABSENCE of a vehicle-busy condition: busy is evaluated
// per-row in Go (see VehicleHasActiveInstantRide) because a busy car must be
// HELD and retried on the next tick, not filtered out and forgotten.
const queryRideRequestListDue = `SELECT ` + rideRequestColumns + `
FROM go_ride_requests
WHERE scheduled_for IS NOT NULL
  AND status = 'accepted'
  AND dispatched_at IS NULL
  AND scheduled_for <= $1
ORDER BY scheduled_for ASC, id ASC
LIMIT $2`

// queryRideRequestVehicleBusy reports whether a vehicle is currently
// committed to an active INSTANT ride — the reservation sweeper's
// vehicle-busy hold (MYR-179). It shares activeInstantRidePredicate with the
// `hasActiveRide` catalog flag, which is character-for-character the
// predicate of `uq_go_ride_requests_active_instant_vehicle` (migration 0013),
// so "the sweeper thinks the car is free" and "the accept guard would let an
// instant ride onto this car" can never disagree.
//
// Postgres answers it from that partial index (the same index-only probe the
// catalog flag measured), so it costs one probe per due reservation.
const queryRideRequestVehicleBusy = `SELECT EXISTS (
	SELECT 1
	FROM go_ride_requests r
	WHERE r.vehicle_id = $1
	  AND ` + activeInstantRidePredicate + `
)`
