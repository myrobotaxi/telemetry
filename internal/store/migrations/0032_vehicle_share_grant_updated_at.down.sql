-- 0032_vehicle_share_grant_updated_at.down.sql
--
-- Drops the MYR-451 mutation timestamp.
--
-- LOSSY AND UNRECOVERABLE, in the one way that matters: the column exists to
-- date capability changes after the fact, so dropping it destroys exactly the
-- forensic record it was added to keep. Re-running the up migration restores
-- the column but stamps every row with the new migration instant — the real
-- history does not come back.
--
-- No enforcement decision reads this column, so the down migration cannot widen
-- anyone's access (contrast 0024's down, which silently restores every
-- suspension and every withdrawn ride capability). It only blinds us again.

ALTER TABLE go_vehicle_shares
    DROP COLUMN IF EXISTS updated_at;
