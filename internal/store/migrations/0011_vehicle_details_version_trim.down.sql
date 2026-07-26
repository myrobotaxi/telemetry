-- 0011_vehicle_details_version_trim.down.sql
--
-- Reverse MYR-279: drop the software_version + trim detail columns from the
-- Go-owned go_vehicle_control_state side table.

ALTER TABLE go_vehicle_control_state
    DROP COLUMN IF EXISTS software_version,
    DROP COLUMN IF EXISTS trim;
