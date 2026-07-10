-- 0003_ride_dispatch.up.sql
--
-- MYR-176: dispatch outcome on go_ride_requests. When an owner accepts a ride
-- request the server pushes the pickup into the vehicle's Tesla navigation
-- (an unsigned navigation_gps_request via the command Executor). The outcome
-- of that push is recorded on the ride row so it is visible on the party-only
-- REST detail (rest-api.md §7.8) without inventing a new main lifecycle
-- status — `accepted` stays `accepted`; these columns are an orthogonal,
-- nullable annotation.
--
-- Idempotency (CG-DL-9): dispatched_at doubles as the exactly-once claim
-- latch. The dispatcher claims a ride with a guarded
-- `... SET dispatched_at = NOW() WHERE id = $1 AND dispatched_at IS NULL`;
-- a re-delivered ride.accepted event finds the latch already set and skips,
-- so nav is pushed at most once per ride.
--
-- Classification (data-classification.md §1.9): all three columns are P0 —
-- an opaque outcome enum, a timestamp, and an opaque error CODE (never a
-- coordinate, address, token, or raw VIN; the dispatcher constructs the code
-- from the typed command error only).

ALTER TABLE go_ride_requests
    ADD COLUMN IF NOT EXISTS dispatch_status TEXT
        CONSTRAINT go_ride_requests_dispatch_status_check CHECK (
            dispatch_status IN ('sent', 'failed', 'skipped')
        ),
    ADD COLUMN IF NOT EXISTS dispatched_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dispatch_error  TEXT;
