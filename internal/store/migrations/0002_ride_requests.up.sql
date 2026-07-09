-- 0002_ride_requests.up.sql
--
-- MYR-173: go_ride_requests — the P10 ride-hailing aggregate. One row per
-- ride request a rider sends to a vehicle owner (contracts
-- schemas/ride-request.schema.json is the wire source of truth; this table
-- is its persisted mirror).
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix. Columns are
-- snake_case (Go-owned tables are not bound to the camelCase style of the
-- Next.js app's schema). rider_id / owner_id reference user cuids and
-- vehicle_id references a vehicle cuid, but NO foreign keys are declared:
-- CG-DL-9 forbids Go migration SQL from referencing tables owned by the
-- sibling app's schema.
--
-- Encryption (NFR-3.23, docs/contracts/data-classification.md §1.9):
-- pickup/dropoff GPS coordinates are P1 and stored ONLY as AES-256-GCM
-- ciphertext (*_enc TEXT columns, base64 [version][nonce][ct+tag]). The
-- table is new, so there is no plaintext column and no dual-write rollout —
-- encrypt-only from day one. Labels/addresses and passenger fields are P1
-- log-redacted but not app-level encrypted (same rationale as the
-- reverse-geocoded strings in §3.2).

CREATE TABLE IF NOT EXISTS go_ride_requests (
    id                      TEXT PRIMARY KEY,
    rider_id                TEXT        NOT NULL,
    owner_id                TEXT        NOT NULL,
    vehicle_id              TEXT        NOT NULL,

    pickup_lat_enc          TEXT        NOT NULL,
    pickup_lng_enc          TEXT        NOT NULL,
    pickup_label            TEXT        NOT NULL,
    pickup_address          TEXT,

    dropoff_lat_enc         TEXT        NOT NULL,
    dropoff_lng_enc         TEXT        NOT NULL,
    dropoff_label           TEXT        NOT NULL,
    dropoff_address         TEXT,

    -- Main lifecycle (contracts $defs.RideRequestStatus). The CHECK keeps
    -- bugs out of the column; adding a lifecycle state is a contract change
    -- and ships with its own migration.
    status                  TEXT        NOT NULL DEFAULT 'requested'
        CONSTRAINT go_ride_requests_status_check CHECK (
            status IN ('requested', 'accepted', 'declined', 'enroute',
                       'arrived', 'completed', 'cancelled')
        ),

    -- Booked-for passenger (contracts passengerName / passengerPhone).
    passenger_name          TEXT,
    passenger_phone         TEXT,

    -- Scheduled rides + the reschedule negotiation (orthogonal sub-state;
    -- contracts $defs.RescheduleStatus). NULL reschedule_status = no
    -- reschedule ever proposed.
    scheduled_for           TIMESTAMPTZ,
    reschedule_proposed_for TIMESTAMPTZ,
    reschedule_status       TEXT
        CONSTRAINT go_ride_requests_reschedule_status_check CHECK (
            reschedule_status IN ('requested', 'confirmed', 'declined')
        ),

    accepted_at             TIMESTAMPTZ,
    completed_at            TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Rider history / scheduled lists: newest first per rider (MYR-174).
CREATE INDEX IF NOT EXISTS idx_go_ride_requests_rider
    ON go_ride_requests (rider_id, created_at DESC, id DESC);

-- Owner incoming feed, filterable by status (MYR-175).
CREATE INDEX IF NOT EXISTS idx_go_ride_requests_owner_status
    ON go_ride_requests (owner_id, status, created_at DESC, id DESC);

-- Per-car dispatch lookups (MYR-176).
CREATE INDEX IF NOT EXISTS idx_go_ride_requests_vehicle
    ON go_ride_requests (vehicle_id, created_at DESC, id DESC);
