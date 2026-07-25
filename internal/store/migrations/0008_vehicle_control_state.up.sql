-- 0008_vehicle_control_state.up.sql
--
-- MYR-269: go_vehicle_control_state — durable last-known owner-control state for
-- the four owner controls the app renders (Lock, Trunk/Frunk, Climate, Charge
-- port). These fields are stream-fed cabin read-backs (MYR-252) that were
-- live-WebSocket-only with NO persistence: on a snapshot (sheet-open) for a
-- non-streaming car (in service / asleep / offline) they were always unknown,
-- so the controls showed "Unavailable" even when the MYR-260 /vehicle_data edge
-- backfill had just republished honest values — those values evaporated unless
-- the phone held a live socket. This table is where the backfill AND the live
-- persist path now land those values durably so a later /snapshot returns them
-- with no live socket (the MYR-253 hydration, via a Go side table rather than a
-- cross-repo Prisma migration).
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case
-- columns. vehicle_id holds a cuid that identifies a row in the sibling app's
-- schema, but NO foreign key is declared: CG-DL-9 forbids Go migration SQL from
-- referencing tables owned by the sibling app's schema. vehicle_id is the
-- primary key so the write path is an idempotent per-car upsert (ON CONFLICT DO
-- UPDATE with per-field COALESCE = last-writer-wins per field) and the snapshot
-- read is a single indexed left-join lookup.
--
-- Every control column is nullable: a NULL means "never read" and the read path
-- surfaces it as absent/unknown (an honest "unavailable"), never a fabricated
-- on/off.
--
-- Classification: all columns are P0 cabin/lock/door state — the same tier as
-- the MYR-252 fields they mirror. Not identifying, no GPS, no tokens, no PII
-- (data-classification.md section 0 / section 2).

CREATE TABLE IF NOT EXISTS go_vehicle_control_state (
    vehicle_id        TEXT        NOT NULL,
    is_locked         BOOLEAN,
    frunk_open        BOOLEAN,
    trunk_open        BOOLEAN,
    is_climate_on     BOOLEAN,
    charge_port_open  BOOLEAN,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (vehicle_id)
);
