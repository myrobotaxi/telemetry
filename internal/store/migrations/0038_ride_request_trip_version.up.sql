-- MYR-541: monotonic version of the ride's TRIP SHAPE (pickup/drop-off, later
-- stops). Incremented by every accepted trip edit, never by lifecycle
-- transitions; the editor echoes the version it holds and a stale edit loses.
-- IF NOT EXISTS because TestMigration0004 re-applies the ladder over an
-- existing schema (the 0034/0036/0037 convention).
ALTER TABLE go_ride_requests ADD COLUMN IF NOT EXISTS trip_version integer NOT NULL DEFAULT 0;
