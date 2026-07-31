-- 0026_ride_vehicle_window_index.down.sql
--
-- Drop the per-vehicle ride-window conflict index (MYR-383). Purely additive
-- migration: no data change to reverse, and the conflict probe still returns
-- the same rows without it (only slower). The GATE itself is not in this index
-- — it is the advisory-lock transaction in internal/store/ride_request_conflict.go
-- — so dropping this weakens performance, never correctness.

DROP INDEX IF EXISTS idx_go_ride_requests_vehicle_window;
