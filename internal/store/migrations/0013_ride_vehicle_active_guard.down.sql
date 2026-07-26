-- 0013_ride_vehicle_active_guard.down.sql
--
-- Drop the one-active-instant-ride-per-vehicle partial unique guard (MYR-266).
-- The dedup (older double-booked rides cancelled) is a data change and is NOT
-- reversed — a cancelled ride stays cancelled.

DROP INDEX IF EXISTS uq_go_ride_requests_active_instant_vehicle;
