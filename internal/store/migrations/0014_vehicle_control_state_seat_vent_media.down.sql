-- 0014_vehicle_control_state_seat_vent_media.down.sql
--
-- Reverse MYR-298: drop the seat-ventilation and media-playback columns from
-- the Go-owned go_vehicle_control_state side table. Reverting leaves the
-- MYR-269 booleans, the MYR-273 cabin settings, the MYR-279 detail columns, and
-- the MYR-274 climate-mode columns intact, but makes seatVentEnabled and
-- mediaPlaybackStatus live-WebSocket-only again -- a client that misses the live
-- frame sees the honest-unknown null on a non-streaming snapshot until another
-- live frame arrives. No data owned by the sibling app's ORM is touched.

ALTER TABLE go_vehicle_control_state
    DROP COLUMN IF EXISTS seat_vent_enabled,
    DROP COLUMN IF EXISTS media_playback_status;
