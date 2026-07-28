-- 0017_vehicle_control_state_service_window.down.sql
--
-- Reverse MYR-316: drop the Tesla-sourced and owner-entered service-window
-- columns from the Go-owned go_vehicle_control_state side table. Reverting
-- leaves the MYR-269 booleans, the MYR-273 cabin settings, the MYR-279 detail
-- columns, the MYR-274 climate-mode columns, the MYR-298 seat-vent/media-
-- playback columns and the MYR-303/308 media now-playing + seat-cooling-
-- capability columns intact.
--
-- After reverting, serviceEstimatedEndAt is permanently null on both
-- /snapshot and GET /api/vehicles. Per the wire contract a null estimate means
-- NO BOUND, so consumers keep scheduling fully open and the server stops
-- refusing scheduled rides that fall inside a service visit -- the pre-MYR-316
-- behaviour, where an owner could accept a reservation for the middle of a
-- service visit. That is a capability regression, not a data-integrity one:
-- the MYR-277 instant-ride in-service gate is unaffected.
--
-- Owner-entered values are LOST (there is nowhere else to keep them) and would
-- have to be re-typed after a re-migrate; Tesla-sourced values are re-read on
-- the next connectivity edge while the car is still in service.
--
-- No data owned by the sibling app's ORM is touched.

ALTER TABLE go_vehicle_control_state
    DROP COLUMN IF EXISTS service_etc,
    DROP COLUMN IF EXISTS service_expected_end_at;
