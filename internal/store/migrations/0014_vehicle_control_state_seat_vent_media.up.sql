-- 0014_vehicle_control_state_seat_vent_media.up.sql
--
-- MYR-298: extend the Go-owned go_vehicle_control_state side table (MYR-269,
-- migration 0008; MYR-273, migration 0010; MYR-279, migration 0011; MYR-274,
-- migration 0012) with the LAST TWO MYR-252 cabin read-backs that were still
-- live-WebSocket-only: seat ventilation (seatVentEnabled, bool) and media
-- playback status (mediaPlaybackStatus, enum "Unknown"/"Stopped"/"Playing"/
-- "Paused"). Both are contracted vehicle_update fields, but neither was
-- persisted NOR emitted on the REST /snapshot, so a client that missed the live
-- frame -- a phone that backgrounded, a car that went to sleep, any socket drop
-- -- could never learn them. This migration is where the live persist path lands
-- those values durably so a later /snapshot returns them with no live socket.
-- This closes out the MYR-253 hydration for the cabin read-back set.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case columns,
-- no foreign key to any table owned by the sibling app's ORM. The columns are
-- added to the existing side table (keyed by vehicle_id) so the same idempotent
-- per-car upsert and the same snapshot LEFT JOIN carry them.
--
-- Both columns are nullable (honest-unknown, matching 0008/0010/0012): a NULL
-- means "never read" and the read path surfaces it as null/unknown, never a
-- fabricated value. seat_vent_enabled is BOOLEAN and a real observation
-- (including false) overwrites. media_playback_status is TEXT holding the wire
-- enum's string form; a streamed "Unknown"/empty is treated as never-read and
-- persists NULL, mirroring how hvac_auto_mode omits an "Unknown" mode and
-- is_climate_on omits an "Unknown" hvacPower.
--
-- Backfill note (MYR-300 coordination): neither field is added to the MYR-260
-- REST /vehicle_data backfill path. Tesla's cached vehicle_data climate subset
-- carries neither value, so there is nothing to backfill from -- and keeping
-- them off that path means the backfill-overwrites-fresher-stream bug tracked
-- separately in MYR-300 cannot reach these two columns.
--
-- Classification: both columns are P0 cabin state -- the same tier as the
-- MYR-252 fields they mirror. Not identifying, no GPS, no tokens, no PII
-- (data-classification.md section 0 / section 1.13).

ALTER TABLE go_vehicle_control_state
    ADD COLUMN IF NOT EXISTS seat_vent_enabled    BOOLEAN,
    ADD COLUMN IF NOT EXISTS media_playback_status TEXT;
