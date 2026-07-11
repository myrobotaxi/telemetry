-- 0004_ride_active_guard.down.sql
--
-- Drop the one-active-instant-ride-per-rider partial unique guard (MYR-230).

DROP INDEX IF EXISTS uq_go_ride_requests_active_instant_rider;
