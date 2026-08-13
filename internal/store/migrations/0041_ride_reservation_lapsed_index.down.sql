-- 0041_ride_reservation_lapsed_index.down.sql
--
-- Drop the lapsed-dispatch sweep index (MYR-555). Purely additive migration:
-- no data change to reverse, and the sweep's second pass still returns the same
-- rows without it (only slower).

DROP INDEX IF EXISTS idx_go_ride_requests_reservation_lapsed;
