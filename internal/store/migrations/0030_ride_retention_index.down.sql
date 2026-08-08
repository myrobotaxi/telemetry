-- 0030_ride_retention_index.down.sql
--
-- Drop the ride-retention index (MYR-447). Purely additive migration: no data
-- change to reverse, and RidePruner.PruneBatch claims exactly the same rows
-- without it — only by scanning and sorting the whole ride table on every batch
-- instead of walking the LIMIT's worth of index entries. Correctness lives in
-- the claim predicates and in the guards the DELETE and the scrub UPDATE repeat,
-- never in this index.

DROP INDEX IF EXISTS idx_go_ride_requests_retention;
