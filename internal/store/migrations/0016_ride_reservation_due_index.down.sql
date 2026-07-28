-- 0016_ride_reservation_due_index.down.sql
--
-- Drop the reservation-dispatch sweep index (MYR-179). Purely additive
-- migration: no data change to reverse, and the sweep query still returns the
-- same rows without it (only slower).

DROP INDEX IF EXISTS idx_go_ride_requests_reservation_due;
