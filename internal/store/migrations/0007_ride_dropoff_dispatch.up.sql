-- 0007_ride_dropoff_dispatch.up.sql
--
-- MYR-265: leg-2 (dropoff) nav-dispatch outcome on go_ride_requests. The
-- autonomous ride has TWO nav legs: leg 1 pushes the PICKUP on owner accept
-- (migration 0005 dispatch_status/dispatched_at/dispatch_error), leg 2 pushes
-- the DROPOFF when the rider boards (POST /api/ride-requests/{id}/board, the
-- guarded accepted→enroute transition). Leg 2 gets its OWN triple of columns
-- rather than overwriting leg 1's so both dispatch histories survive
-- independently — a failed dropoff push must not erase the record that the
-- pickup push succeeded (and vice-versa). The two legs are otherwise identical
-- in shape; the dispatcher generalizes one pipeline over both.
--
-- Idempotency (CG-DL-9), mirroring leg 1: dropoff_dispatched_at doubles as the
-- exactly-once claim latch. The dispatcher claims with a guarded
-- `... SET dropoff_dispatched_at = NOW() WHERE id = $1 AND dropoff_dispatched_at IS NULL`;
-- a re-delivered ride.boarded event finds the latch already set and skips, so
-- the dropoff nav is pushed at most once per board. (The board transition guard
-- — accepted→enroute wins exactly once — is the primary exactly-once gate; this
-- latch is the belt-and-suspenders DB guard, symmetric with leg 1.)
--
-- Classification (data-classification.md §1.9): all three columns are P0 — an
-- opaque outcome enum, a timestamp, and an opaque error CODE (never a
-- coordinate, address, token, or raw VIN; constructed from the typed command
-- error only). Not exposed on the wire.
--
-- NOTE: a startup reconciler for interrupted LEG-2 dispatches is intentionally
-- NOT added here (leg 1 has one via the dispatch_status IS NULL sweep). A
-- crash in the dropoff claim→record window leaves dropoff_dispatched_at set /
-- dropoff_dispatch_status NULL; impact is low (the car keeps its valid pickup
-- nav and the ride still completes on drive-end), and a follow-up can extend
-- the reconciler if leg-2 interruptions prove material in production.
--
-- enroute_at (drive-end correlation): stamped in the SAME guarded UPDATE that
-- transitions accepted→enroute (the board path), enroute_at pins WHEN leg 2
-- began. The drive-end completer only completes a ride when the ENDED drive
-- STARTED AT/AFTER enroute_at (drive_started_at >= enroute_at) — so a DELAYED
-- leg-1 (pickup) drive-end debounce, whose drive started well before board,
-- can no longer false-complete the ride at the pickup while it is already
-- enroute (reviewer edge 4d). Consequence kept on purpose: if the leg-2 push
-- failed and the car never departs, there is no leg-2 drive, so the ride stays
-- enroute (open) rather than false-completing — the honest v1 behavior. P0,
-- off-wire.

ALTER TABLE go_ride_requests
    ADD COLUMN IF NOT EXISTS dropoff_dispatch_status TEXT
        CONSTRAINT go_ride_requests_dropoff_dispatch_status_check CHECK (
            dropoff_dispatch_status IN ('sent', 'failed', 'skipped')
        ),
    ADD COLUMN IF NOT EXISTS dropoff_dispatched_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dropoff_dispatch_error  TEXT,
    ADD COLUMN IF NOT EXISTS enroute_at              TIMESTAMPTZ;
