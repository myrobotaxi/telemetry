-- 0015_vehicle_control_state_media_now_playing.down.sql
--
-- Reverse MYR-303 + MYR-308: drop the media now-playing columns and the
-- ventilated-seat capability column from the Go-owned go_vehicle_control_state
-- side table. Reverting leaves the MYR-269 booleans, the MYR-273 cabin
-- settings, the MYR-279 detail columns, the MYR-274 climate-mode columns and
-- the MYR-298 seat-vent/media-playback columns intact.
--
-- After reverting, the eight MYR-303 media fields become live-WebSocket-only
-- again: a client that misses the live frame sees the honest-unknown null on a
-- non-streaming snapshot until another live frame arrives. seat_cooling_capable
-- has no live stream to fall back on at all (Tesla does not stream it), so it
-- reverts to permanently null and clients fall back to the pre-MYR-308
-- telemetry-presence heuristic -- treat the car as capable when
-- seatCoolerLeft/Right have ever been non-null -- which is exactly the
-- behaviour vehicle-state.schema.json mandates for an absent value.
--
-- No data owned by the sibling app's ORM is touched.

ALTER TABLE go_vehicle_control_state
    DROP COLUMN IF EXISTS media_now_playing_title,
    DROP COLUMN IF EXISTS media_now_playing_artist,
    DROP COLUMN IF EXISTS media_now_playing_album,
    DROP COLUMN IF EXISTS media_now_playing_station,
    DROP COLUMN IF EXISTS media_playback_source,
    DROP COLUMN IF EXISTS media_now_playing_duration_ms,
    DROP COLUMN IF EXISTS media_now_playing_elapsed_ms,
    DROP COLUMN IF EXISTS media_volume_max,
    DROP COLUMN IF EXISTS seat_cooling_capable;
