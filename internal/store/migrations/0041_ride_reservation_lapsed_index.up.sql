-- 0041_ride_reservation_lapsed_index.up.sql
--
-- MYR-555: index the LAPSED-dispatch sweep — the second half of the lateness
-- ceiling, for reservations that WERE dispatched and never became a ride.
--
-- Why the sweep needs a second pass at all: migration 0016's index and the due
-- query it serves are both partial on `dispatched_at IS NULL`, which is what
-- makes scheduled dispatch self-terminating — a claimed row can never be
-- re-selected. The cost was that a reservation whose pickup nav succeeded and
-- whose rider never got in the car left the candidate set the instant it was
-- claimed and had no path to ANY resolution: it sat `accepted` forever, with a
-- rider Live Activity counting down to a pickup that already came and went and
-- nothing left in the system able to end it (prod ride c92b2c86 — scheduled
-- 21:45Z, dispatched 21:40:24Z, still `accepted` hours later).
--
-- This index is the exact COMPLEMENT of 0016's, and its predicate is
-- lapsedDispatchPredicate character-for-character:
--
--   scheduled_for IS NOT NULL  — reservations only; an instant ride's accept is
--                                its due instant and has no ceiling to miss.
--   status = 'accepted'        — the ride never became one. A ride the parties
--                                proceeded with manually, or cancelled, leaves
--                                the set on its own.
--   dispatched_at IS NOT NULL  — the leg-1 latch IS claimed. The complement of
--                                0016, and the reason these rows were invisible.
--   dispatch_error IS NOT
--     'reservation_expired'    — the verdict has not been recorded yet. This is
--                                what makes the set SELF-DRAINING: the resolving
--                                UPDATE stamps the very column excluded here, so
--                                each row is resolved exactly once and never
--                                re-listed.
--
-- The residual set is therefore tiny and drains on its own, exactly like 0016's
-- — which is what keeps a 30-second loop over a table that retains terminal
-- rides indefinitely at a few index tuples rather than a growing scan.
--
-- COALESCE rather than `dispatch_error IS DISTINCT FROM ...` so the index
-- predicate and the query predicate are textually identical, which is what the
-- planner's implication prover matches on.
--
-- Purely additive: one partial index, no column, no data change, no new status
-- value. Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case.
-- Classification: an index over existing P0 columns, not exposed on the wire.

CREATE INDEX IF NOT EXISTS idx_go_ride_requests_reservation_lapsed
    ON go_ride_requests (scheduled_for)
    WHERE scheduled_for IS NOT NULL
      AND status = 'accepted'
      AND dispatched_at IS NOT NULL
      AND COALESCE(dispatch_error, '') <> 'reservation_expired';
