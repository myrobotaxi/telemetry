-- 0012_vehicle_control_state_climate_mode.up.sql
--
-- MYR-274: extend the Go-owned go_vehicle_control_state side table (MYR-269,
-- migration 0008; MYR-273, migration 0010; MYR-279, migration 0011) with the two
-- climate-MODE read-backs that back the owner climate section's Auto / Cool / Heat
-- segmented control: the HVAC auto mode (hvacAutoMode enum: "On"/"Override"/
-- "Unknown") and whether the A/C is enabled (hvacAcEnabled bool). Like the five
-- MYR-269 owner-control booleans and the eleven MYR-273 cabin settings, these are
-- stream-fed cabin read-backs (MYR-252) that were live-WebSocket-only with NO
-- persistence: on a snapshot (sheet-open) for a non-streaming car (in service /
-- asleep / offline) the Auto/Cool/Heat segment could not reflect the car's real
-- mode until a live frame arrived, because the values evaporated the moment the
-- phone dropped its live socket. This migration is where the live persist path AND
-- the MYR-260 /vehicle_data backfill now land those values durably so a later
-- /snapshot returns them with no live socket. This completes the MYR-253 hydration
-- for the climate mode.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case columns,
-- no foreign key to any Prisma-owned table. The columns are added to the existing
-- side table (keyed by vehicle_id) so the same idempotent per-car upsert and the
-- same snapshot LEFT JOIN carry them.
--
-- Both columns are nullable (honest-unknown, matching 0008/0010): a NULL means
-- "never read" and the read path surfaces it as absent/unknown, never a fabricated
-- mode. hvac_auto_mode is TEXT (the wire enum's string form, e.g. "On"/"Override";
-- a streamed "Unknown"/empty is treated as never-read and persists NULL, mirroring
-- how is_climate_on OMITS an "Unknown" hvacPower). hvac_ac_enabled is BOOLEAN, and
-- a real observation (including false) overwrites.
--
-- Classification: both columns are P0 cabin state -- the same tier as the MYR-252
-- fields they mirror. Not identifying, no GPS, no tokens, no PII
-- (data-classification.md section 0 / section 2).

ALTER TABLE go_vehicle_control_state
    ADD COLUMN IF NOT EXISTS hvac_auto_mode  TEXT,
    ADD COLUMN IF NOT EXISTS hvac_ac_enabled BOOLEAN;
