-- 0006_removed_vehicles.up.sql
--
-- MYR-261: go_removed_vehicles — the per-owner removed-VIN tombstone. When an
-- owner tears down a car (MYR-258 owner-offboarding), the local delete removed
-- the identity row but left NO durable record of the removal, so the very next
-- Tesla re-link's best-effort vehicle sync (owner_stream_hook.AfterLink to
-- UpsertOwnedVehicle, an ON CONFLICT upsert) re-inserted the still-Tesla-owned
-- id and the car reappeared. This table is that missing record: one row per
-- (owner, Tesla vehicle id) the owner has removed. The sync path consults it
-- and SKIPS any tombstoned id, so a passive re-link can never resurrect a
-- removed car. A deliberate re-add clears the row (see ClearTombstone).
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case
-- columns. user_id references a user cuid and tesla_vehicle_id is the Tesla
-- fleet vehicle identifier, but NO foreign keys are declared: CG-DL-9 forbids
-- Go migration SQL from referencing tables owned by the sibling app's schema.
-- The natural composite key (user_id, tesla_vehicle_id) is the primary key, so
-- writing a tombstone is idempotent (ON CONFLICT DO UPDATE refreshes
-- removed_at) and the sync-path existence check is a single indexed lookup.
--
-- Classification: all columns are P0 — opaque owner cuid, opaque Tesla vehicle
-- id, a redactable vin kept for operator correlation, and a timestamp. No GPS,
-- tokens, addresses, or PII (data-classification.md section 2).

CREATE TABLE IF NOT EXISTS go_removed_vehicles (
    user_id           TEXT        NOT NULL,
    tesla_vehicle_id  TEXT        NOT NULL,
    vin               TEXT,
    removed_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (user_id, tesla_vehicle_id)
);
