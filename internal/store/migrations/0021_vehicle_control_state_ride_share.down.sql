-- 0021_vehicle_control_state_ride_share.down.sql
--
-- Reverse MYR-342: drop the owner ride-sharing switch from the Go-owned
-- go_vehicle_control_state side table. Reverting leaves the MYR-269 booleans,
-- the MYR-273 cabin settings, the MYR-279 detail columns, the MYR-274
-- climate-mode columns, the MYR-298 seat-vent/media-playback columns, the
-- MYR-303/308 media now-playing + seat-cooling-capability columns, the MYR-316
-- service-window timestamps and the MYR-320 detail strings intact.
--
-- After reverting, rideShareEnabled is permanently absent from GET /api/vehicles
-- and the REST /snapshot. Per the wire contract an ABSENT value means ENABLED,
-- so every consumer reverts to showing every car as accepting rides, and the
-- three server-side gates (ride-request create, owner accept, reservation
-- sweeper) stop refusing anything -- the pre-MYR-342 behaviour.
--
-- THAT IS A CAPABILITY REGRESSION WITH A SAFETY EDGE, and it is the reason this
-- header is longer than 0017's. Every owner who had PAUSED their car is
-- silently un-paused: their vehicle re-enters the rider-facing catalog as
-- available and starts taking requests again without them acting. The paused
-- state is not recoverable either -- there is nowhere else to keep it, so the
-- values are LOST and each owner would have to toggle their car off again after
-- a re-migrate. Do not run this on production while any owner is relying on the
-- pause; prefer rolling the application back and leaving the column in place
-- (an unread NOT NULL DEFAULT true column is inert).
--
-- No data owned by the sibling app's ORM is touched.

ALTER TABLE go_vehicle_control_state
    DROP COLUMN IF EXISTS ride_share_enabled;
