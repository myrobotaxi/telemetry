-- 0010_vehicle_control_state_cabin_settings.down.sql
--
-- MYR-273 rollback: drop the cabin-setting columns added to the Go-owned
-- go_vehicle_control_state side table. Reverting leaves the five MYR-269
-- owner-control booleans intact but makes the cabin settings (temp/fan/seat/
-- volume) live-WebSocket-only again — they show "—" on a non-streaming snapshot.
-- No Prisma-owned data is touched.

ALTER TABLE go_vehicle_control_state
    DROP COLUMN IF EXISTS driver_temp_setting,
    DROP COLUMN IF EXISTS passenger_temp_setting,
    DROP COLUMN IF EXISTS fan_speed,
    DROP COLUMN IF EXISTS seat_heater_left,
    DROP COLUMN IF EXISTS seat_heater_right,
    DROP COLUMN IF EXISTS seat_heater_rear_left,
    DROP COLUMN IF EXISTS seat_heater_rear_center,
    DROP COLUMN IF EXISTS seat_heater_rear_right,
    DROP COLUMN IF EXISTS seat_cooler_left,
    DROP COLUMN IF EXISTS seat_cooler_right,
    DROP COLUMN IF EXISTS media_volume;
