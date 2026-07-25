-- 0010_vehicle_control_state_cabin_settings.up.sql
--
-- MYR-273: extend the Go-owned go_vehicle_control_state side table (MYR-269,
-- migration 0008) with the cabin SETTING read-backs the owner sheet renders —
-- SET TEMP (driver/passenger setpoints), Fan speed, the front/rear seat heater
-- and front seat cooler levels, and the media volume. Like the five MYR-269
-- owner-control booleans these are stream-fed cabin read-backs (MYR-252) that
-- were live-WebSocket-only with NO persistence: on a snapshot (sheet-open) for a
-- non-streaming car (in service / asleep / offline) they showed "—" because the
-- values evaporated the moment the phone dropped its live socket. MYR-272 (iOS)
-- now folds them on LIVE deltas, but on a snapshot they were still unknown. This
-- migration is where the live persist path AND the MYR-260 /vehicle_data backfill
-- now land those values durably so a later /snapshot returns them with no live
-- socket. This completes the MYR-253 hydration for the cabin settings.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case columns,
-- no foreign key to any Prisma-owned table. The columns are added to the existing
-- side table (keyed by vehicle_id) so the same idempotent per-car upsert and the
-- same snapshot LEFT JOIN carry them.
--
-- Every column is nullable: a NULL means "never read" and the read path surfaces
-- it as absent/unknown (an honest "—"), never a fabricated value. The temp/fan/
-- seat levels are integers (fan 0+, seats 0-3, temps Fahrenheit-rounded ints);
-- media_volume is a fractional level (typically 0-11) so it is DOUBLE PRECISION,
-- matching the wire contract (vehicle-state.schema.json mediaVolume: number).
--
-- Classification: all columns are P0 cabin state — the same tier as the MYR-252
-- fields they mirror. Not identifying, no GPS, no tokens, no PII
-- (data-classification.md section 0 / section 2).

ALTER TABLE go_vehicle_control_state
    ADD COLUMN IF NOT EXISTS driver_temp_setting     INT,
    ADD COLUMN IF NOT EXISTS passenger_temp_setting  INT,
    ADD COLUMN IF NOT EXISTS fan_speed               INT,
    ADD COLUMN IF NOT EXISTS seat_heater_left        INT,
    ADD COLUMN IF NOT EXISTS seat_heater_right       INT,
    ADD COLUMN IF NOT EXISTS seat_heater_rear_left   INT,
    ADD COLUMN IF NOT EXISTS seat_heater_rear_center INT,
    ADD COLUMN IF NOT EXISTS seat_heater_rear_right  INT,
    ADD COLUMN IF NOT EXISTS seat_cooler_left        INT,
    ADD COLUMN IF NOT EXISTS seat_cooler_right       INT,
    ADD COLUMN IF NOT EXISTS media_volume            DOUBLE PRECISION;
