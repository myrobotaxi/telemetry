-- 0034_vehicle_nav_reading_at.down.sql
--
-- Drop the MYR-409 nav-reading stamp.
--
-- Rolling this back re-opens the defect rather than merely removing a column:
-- with no nav_reading_at, the leg and ride-context projections fall back to the
-- row stamp and the Arriving rung can be burnt again by a car navigating
-- elsewhere at accept. The data loss itself is harmless -- the column is
-- re-acquired from the next navigation frame each car emits.

ALTER TABLE go_vehicle_control_state
    DROP COLUMN IF EXISTS nav_reading_at;
