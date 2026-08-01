-- 0028_ride_active_poll_index.down.sql
--
-- Drop the active-ride poll-target index (MYR-394). Purely additive migration:
-- no data change to reverse, and ListActiveRidePollTargets returns exactly the
-- same rows without it — only by scanning and sorting the whole ride table
-- instead of walking ~50 index entries. Correctness lives in the predicate, not
-- in this index.

DROP INDEX IF EXISTS idx_go_ride_requests_active_poll;
