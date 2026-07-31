-- 0027_live_activity_progress.down.sql
--
-- Drop the Live Activity leg-progress anchor (MYR-398).
--
-- Safe to run against a live system: the columns hold only rendering memory for
-- Activities currently on somebody's lock screen, never a fact about a ride. A
-- server rolled back past this migration stops sending `progress`, the field is
-- optional on the wire, and the card degrades to the trackless rendering it had
-- before r16 — a headline and a meet-at line, which is exactly what an older
-- app build shows anyway.

ALTER TABLE go_live_activities
    DROP COLUMN IF EXISTS progress_leg,
    DROP COLUMN IF EXISTS progress_source,
    DROP COLUMN IF EXISTS progress_baseline,
    DROP COLUMN IF EXISTS progress_value;
