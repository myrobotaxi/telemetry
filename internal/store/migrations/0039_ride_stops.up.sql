-- 0039_ride_stops.up.sql
--
-- MYR-539: go_ride_stops — the INTERMEDIATE stops of a multi-stop trip
-- (contracts v0.34.0 $defs.RideStop; CLIENT-DIRECTED: exactly ONE pickup → N
-- ordered stops → ONE destination). The endpoints are NOT members of this
-- table: they stay go_ride_requests.pickup_* / dropoff_*, so a row here is
-- always the MIDDLE of a trip.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case, and
-- the ONE foreign key it declares points at another GO-OWNED table
-- (go_ride_requests) — never at anything the sibling app's Prisma schema
-- owns. ON DELETE CASCADE is what keeps the retention sweep honest: the
-- MYR-447 pruner deletes terminal ride rows directly, and a stop row that
-- outlived its ride would be P1 location data with no owner and nothing left
-- to delete it.
--
-- Encryption (NFR-3.23, data-classification.md §1.9): a stop's coordinates
-- are P1 GPS data and are stored ONLY as AES-256-GCM ciphertext, exactly like
-- the ride's own pickup/dropoff pair — same column shape (*_enc TEXT), same
-- repo-level encrypt/decrypt boundary, no plaintext column and therefore no
-- dual-write rollout. label/address are P1 log-redacted but not app-level
-- encrypted (the endpoints' rationale, unchanged).
--
-- POSITION IS THE TRAVEL ORDER and it is dense (0, 1, 2, …): the read
-- projection is "ORDER BY position", so contracts' `stops` array index IS this
-- column and a reorder is just a re-numbering. There is deliberately no UNIQUE
-- constraint on (ride_id, position): the replacement write re-numbers the whole
-- list inside one transaction, and a uniqueness check that fired on an
-- INTERMEDIATE state of that re-numbering would refuse edits that are perfectly
-- legal once the statement set completes.
--
-- STATUS is SERVER-OWNED (never client-written) and advances on arrival
-- detection — the MYR-538 waypoint events. The CHECK keeps bugs out of the
-- column the same way go_ride_requests_status_check does; 'upcoming' is the
-- default because a stop is born ahead of the car.
--
-- IF NOT EXISTS throughout, the 0034/0036/0037/0038 convention: TestMigration0004
-- re-applies the ladder over an existing schema.

CREATE TABLE IF NOT EXISTS go_ride_stops (
    id          TEXT PRIMARY KEY,
    ride_id     TEXT        NOT NULL
        REFERENCES go_ride_requests (id) ON DELETE CASCADE,
    position    INTEGER     NOT NULL,

    lat_enc     TEXT        NOT NULL,
    lng_enc     TEXT        NOT NULL,
    label       TEXT        NOT NULL,
    address     TEXT,

    status      TEXT        NOT NULL DEFAULT 'upcoming'
        CONSTRAINT go_ride_stops_status_check CHECK (
            status IN ('upcoming', 'current', 'completed')
        ),

    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The only access path there is: "give me this ride's stops, in travel order".
-- It serves the detail read, the batched list read (ride_id = ANY(...)), the
-- replacement write's FOR UPDATE snapshot, and the leg-advance's "next stop
-- after this position" probe — all four are (ride_id, position).
CREATE INDEX IF NOT EXISTS idx_go_ride_stops_ride_position
    ON go_ride_stops (ride_id, position);
