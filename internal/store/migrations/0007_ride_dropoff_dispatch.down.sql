-- 0007_ride_dropoff_dispatch.down.sql — reverse of the up migration (MYR-265).
ALTER TABLE go_ride_requests
    DROP COLUMN IF EXISTS dropoff_dispatch_status,
    DROP COLUMN IF EXISTS dropoff_dispatched_at,
    DROP COLUMN IF EXISTS dropoff_dispatch_error,
    DROP COLUMN IF EXISTS enroute_at;
