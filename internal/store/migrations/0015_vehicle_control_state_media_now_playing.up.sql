-- 0015_vehicle_control_state_media_now_playing.up.sql
--
-- MYR-303 + MYR-308: extend the Go-owned go_vehicle_control_state side table
-- (MYR-269, migration 0008; MYR-273, migration 0010; MYR-279, migration 0011;
-- MYR-274, migration 0012; MYR-298, migration 0014) with the media NOW-PLAYING
-- block and the ventilated-seat CAPABILITY bit.
--
-- MYR-303 adds the eight streamed media fields that turn the existing
-- play/pause + volume read-backs into an actual now-playing panel: what is
-- playing (title/artist/album), where from (station = the channel WITHIN a
-- source; playback source = the app/input doing the playing), how long
-- (duration/elapsed, milliseconds) and the per-vehicle volume ceiling.
--
-- MYR-308 adds seat_cooling_capable, which is NOT telemetry at all: it is a
-- SPEC fact read from Tesla REST vehicle_data.vehicle_config.has_seat_cooling.
-- It lands in this table for the same reason trim (migration 0011) did -- there
-- is no home for it in the sibling app's ORM-owned "Vehicle" table, and the
-- snapshot LEFT JOIN already carries this table.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case
-- columns, no foreign key to any table owned by the sibling app's ORM. Columns
-- are added to the existing side table (keyed by vehicle_id) so the same
-- idempotent per-car upsert and the same snapshot LEFT JOIN carry them.
--
-- Every column is nullable (honest-unknown, matching 0008/0010/0012/0014): NULL
-- means "never read" and the read path surfaces it as null, never a fabricated
-- value.
--
-- EMPTY STRING vs NULL -- the deliberate divergence from MYR-298.
-- media_playback_status (0014) treats an empty OR "Unknown" value as never-read
-- and persists NULL, because it is an ENUM whose "Unknown" member literally
-- means "we could not read this" -- letting it through would overwrite a known
-- status with an admission of ignorance.
--
-- The five TEXT columns added here are FREE TEXT, not an enum, and they carry
-- the opposite meaning: an empty title/artist/album/station/source is the car
-- telling us the track ENDED and nothing is playing now. That is a real
-- observation about the world, and it MUST overwrite a stale known value or the
-- panel would advertise a song that stopped playing an hour ago. So an empty
-- string is persisted AS an empty string (non-NULL, and therefore wins the
-- COALESCE upsert), and only a genuinely absent field stays NULL. The read path
-- keeps the two distinguishable: '' is "nothing playing", NULL is "never
-- observed".
--
-- The three numeric media columns follow the same rule as every other numeric
-- level in this table: a real observation INCLUDING zero overwrites; absent
-- stays NULL. media_now_playing_duration_ms may legitimately hold Tesla's
-- 18000000 (5h) radio sentinel -- that is a real emitted value, stored as-is,
-- and it is the CLIENT's job to render it as "no duration" rather than a
-- five-hour track (see vehicle-state.schema.json).
--
-- Types: the two millisecond counters are BIGINT (a track length in ms exceeds
-- nothing near INT range in practice, but ms counters are exactly the kind of
-- value that should never be one firmware bug away from overflow).
-- media_volume_max is DOUBLE PRECISION, matching the media_volume column it
-- bounds -- the contract types both as `number`, not `integer`, so that
-- mediaVolume / mediaVolumeMax stays exact.
--
-- Backfill note (MYR-300 coordination): none of the eight MYR-303 media columns
-- is fed by the MYR-260 REST /vehicle_data backfill -- Tesla's cached
-- vehicle_data carries no now-playing block -- so the backfill-overwrites-
-- fresher-stream defect cannot reach them. seat_cooling_capable is the exact
-- inverse: it is fed ONLY by that backfill and never by the stream, which is
-- why it is not in fieldMap and therefore never dropped by the MYR-300
-- stream-recency gate.
--
-- Classification: the five free-text columns are P1 -- an accumulated
-- title/artist/album/station/source stream reveals listening habits. The three
-- numeric media columns are P0 (a bare length/offset/ceiling identifies
-- nothing), as is seat_cooling_capable (an equipment fact, the same tier as
-- trim). See data-classification.md section 1.13.

ALTER TABLE go_vehicle_control_state
    ADD COLUMN IF NOT EXISTS media_now_playing_title       TEXT,
    ADD COLUMN IF NOT EXISTS media_now_playing_artist      TEXT,
    ADD COLUMN IF NOT EXISTS media_now_playing_album       TEXT,
    ADD COLUMN IF NOT EXISTS media_now_playing_station     TEXT,
    ADD COLUMN IF NOT EXISTS media_playback_source         TEXT,
    ADD COLUMN IF NOT EXISTS media_now_playing_duration_ms BIGINT,
    ADD COLUMN IF NOT EXISTS media_now_playing_elapsed_ms  BIGINT,
    ADD COLUMN IF NOT EXISTS media_volume_max              DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS seat_cooling_capable          BOOLEAN;
