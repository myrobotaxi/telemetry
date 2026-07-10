-- 0003_ride_dispatch.down.sql — reverse of 0003_ride_dispatch.up.sql (MYR-176).

ALTER TABLE go_ride_requests
    DROP COLUMN IF EXISTS dispatch_error,
    DROP COLUMN IF EXISTS dispatched_at,
    DROP COLUMN IF EXISTS dispatch_status;
