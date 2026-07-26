-- 0011_vehicle_details_version_trim.up.sql
--
-- MYR-279: extend the Go-owned go_vehicle_control_state side table (MYR-269,
-- migration 0008; MYR-273, migration 0010) with the two owner-facing vehicle
-- DETAIL read-backs the app renders on the vehicle-details sheet that the
-- Prisma-owned "Vehicle" table does not carry: the installed SOFTWARE VERSION
-- (Tesla firmware string, e.g. "2026.20.1") and the TRIM badge (e.g.
-- "Performance"). Both were decoded/available upstream but dropped:
--   - software version streams as Tesla proto field Version and is also present
--     in REST vehicle_data.vehicle_state.car_version, but had no store column;
--   - trim is only in REST vehicle_data.vehicle_config.trim_badging and was not
--     plucked at all.
-- Like the MYR-269/273 cabin read-backs these land here (a Go side table) rather
-- than in the Prisma-owned "Vehicle" table so no cross-repo Prisma migration is
-- needed (MYR-253 hydration pattern), and they are returned on the DB-backed
-- REST /snapshot via VehicleRepo.GetByID's existing LEFT JOIN.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case columns,
-- no foreign key to any Prisma-owned table. The columns are added to the existing
-- side table (keyed by vehicle_id) so the same idempotent per-car upsert and the
-- same snapshot LEFT JOIN carry them.
--
-- Both columns are nullable TEXT: a NULL means "never read" and the read path
-- surfaces it as absent (an honest "--"), never a fabricated value. Software
-- version populates from the live stream OR the MYR-260 /vehicle_data backfill;
-- trim populates only from the /vehicle_data backfill (Tesla does not stream it).
--
-- Classification: both columns are P0 vehicle-detail state -- publicly-legible
-- attributes (a firmware string / a trim badge on the car), not identifying, no
-- GPS, no tokens, no PII (data-classification.md section 0 / section 1.9a).

ALTER TABLE go_vehicle_control_state
    ADD COLUMN IF NOT EXISTS software_version TEXT,
    ADD COLUMN IF NOT EXISTS trim             TEXT;
