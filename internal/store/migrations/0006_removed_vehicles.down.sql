-- 0006_removed_vehicles.down.sql
--
-- Reverts MYR-261: drops the Go-owned go_removed_vehicles tombstone table (the
-- composite primary-key index drops with it). Reverting re-opens the
-- reappearance bug, so this is a schema rollback only — not an operational step.

DROP TABLE IF EXISTS go_removed_vehicles;
