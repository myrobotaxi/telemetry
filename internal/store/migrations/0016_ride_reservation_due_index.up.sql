-- 0016_ride_reservation_due_index.up.sql
--
-- MYR-179: index the reservation-dispatch sweep. The sweeper runs
-- queryRideRequestListDue on EVERY replica every 30 seconds, forever, and its
-- steady state is "nothing is due" — the query must therefore cost a few index
-- tuples, not a scan of the whole ride table. go_ride_requests retains terminal
-- rows (completed/cancelled/declined) indefinitely, so an unindexed predicate
-- degrades monotonically with lifetime ride volume until the pass exceeds the
-- 30s sweep timeout and scheduled dispatch stops entirely, log-only.
--
-- The index predicate is EXACTLY the sweep query's three static conjuncts:
--
--   scheduled_for IS NOT NULL  — reservations only (an instant ride dispatches
--                                on accept and is never swept).
--   status = 'accepted'        — the owner committed and the ride has not moved
--                                on.
--   dispatched_at IS NULL      — the leg-1 latch is unclaimed.
--
-- so it is PARTIAL over the tiny, self-draining set of not-yet-dispatched
-- reservations rather than over every ride ever booked. The indexed column is
-- scheduled_for, which serves both the remaining range conjunct
-- (scheduled_for <= now) and the ORDER BY scheduled_for ASC — one index scan,
-- no sort.
--
-- Note it does NOT include the vehicle-busy NOT EXISTS the sweep query also
-- carries: that subquery is answered by uq_go_ride_requests_active_instant_vehicle
-- (migration 0013), which is already exactly the per-vehicle busy predicate.
--
-- Why the existing indexes cannot serve this: idx_go_ride_requests_owner_status
-- leads on owner_id (unconstrained here), idx_go_ride_requests_vehicle leads on
-- vehicle_id (likewise), and the two partial UNIQUE indexes (0004, 0013) are
-- partial on the OPPOSITE predicate `scheduled_for IS NULL`.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case; no
-- Prisma-owned table is referenced. Classification: no new columns; an index
-- over existing P0/P1 columns, not exposed on the wire.

CREATE INDEX IF NOT EXISTS idx_go_ride_requests_reservation_due
    ON go_ride_requests (scheduled_for)
    WHERE scheduled_for IS NOT NULL
      AND status = 'accepted'
      AND dispatched_at IS NULL;
