-- 0029_live_activity_alerted_phase.down.sql
--
-- Drop the Live Activity island-expand high-water mark (MYR-398).
--
-- Safe to run against a live system, with one honest caveat worth naming: the
-- column holds only which expansions a phone has already been shown, never a
-- fact about a ride, so a server rolled back past this migration simply stops
-- attaching alert dictionaries and the islands stop expanding — the cards
-- themselves keep updating exactly as before. The caveat is the roll FORWARD
-- afterwards: the marks are gone, so the re-applied migration's backfill
-- re-seeds them from ride status, and any Activity mid-pickup inside the
-- two-minute threshold gets one more Arriving expansion than it strictly
-- earned. One extra island opening on a subset of in-flight rides is the whole
-- cost of this being reversible.

ALTER TABLE go_live_activities
    DROP COLUMN IF EXISTS alerted_phase;
