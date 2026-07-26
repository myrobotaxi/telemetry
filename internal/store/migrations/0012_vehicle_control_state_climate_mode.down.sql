-- 0012_vehicle_control_state_climate_mode.down.sql
--
-- Reverse MYR-274: drop the climate-mode columns from the Go-owned
-- go_vehicle_control_state side table. Reverting leaves the MYR-269 booleans,
-- the MYR-273 cabin settings, and the MYR-279 detail columns intact but makes
-- the climate mode (hvacAutoMode / hvacAcEnabled) live-WebSocket-only again — the
-- Auto/Cool/Heat segment shows the honest-unknown state on a non-streaming
-- snapshot until a live frame arrives. No Prisma-owned data is touched.

ALTER TABLE go_vehicle_control_state
    DROP COLUMN IF EXISTS hvac_auto_mode,
    DROP COLUMN IF EXISTS hvac_ac_enabled;
