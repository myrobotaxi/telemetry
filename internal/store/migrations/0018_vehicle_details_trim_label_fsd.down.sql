-- 0018_vehicle_details_trim_label_fsd.down.sql
--
-- Reverse MYR-320: drop the human-readable trim label and the FSD software
-- designation from the Go-owned go_vehicle_control_state side table. Reverting
-- leaves the MYR-269 booleans, the MYR-273 cabin settings, the MYR-279 detail
-- columns (software_version, trim), the MYR-274 climate-mode columns, the
-- MYR-298 seat-vent/media-playback columns, the MYR-303/308 media now-playing +
-- seat-cooling-capability columns and the MYR-316 service-window columns intact.
--
-- After reverting, trimLabel and fsdVersion are permanently null on /snapshot.
-- Per the wire contract an absent trimLabel means the consumer renders
-- '<year> <model>' with no label and no dangling separator, and an absent
-- fsdVersion means the details sheet OMITS that row entirely -- both are
-- contract-defined normal states, so this is a feature regression with no
-- broken rendering and no data-integrity consequence. The sibling `trim` (raw
-- badge code) and `software_version` (firmware build) are untouched and keep
-- flowing.
--
-- Stored values are LOST, but nothing is unrecoverable: both are re-read from
-- Tesla on the next non-waking read -- a connectivity edge, an owner-triggered
-- refresh, or the MYR-320 periodic in-service pass -- after a re-migrate.
--
-- The MYR-320 exterior-colour write is NOT affected by this migration in either
-- direction: it targets the pre-existing Prisma-owned "Vehicle".color column
-- and ships no schema change at all.
--
-- No data owned by the sibling app's ORM is touched.

ALTER TABLE go_vehicle_control_state
    DROP COLUMN IF EXISTS trim_label,
    DROP COLUMN IF EXISTS fsd_version;
