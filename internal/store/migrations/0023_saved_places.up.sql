-- 0023_saved_places.up.sql
--
-- MYR-321: go_saved_places — the account-persisted Home and Work places the
-- ride-request sheet's shortcut row has always pretended to remember.
--
-- The gap it closes is the same shape as MYR-349's: a lie, not a missing
-- feature. The two shortcut chips have shipped since the ride-request sheet
-- landed, backed by a private SwiftUI @State struct. They rendered, they
-- tapped, they persisted nowhere. "Home" meant one address on somebody's
-- iPhone and a different one on their iPad, and reinstalling the app forgot
-- both. This table is the missing storage, and the rest-api.md 7.20 endpoints
-- are the only thing that reads or writes it.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case
-- columns, and NO foreign key to any table owned by the sibling app's ORM --
-- user_id holds a cuid identifying a row in that schema and is a plain column
-- here. No sibling-owned relation is named anywhere in this file.
--
-- ACCOUNT-SCOPED, NOT VEHICLE-SCOPED. A saved place belongs to a PERSON, so it
-- follows them across every car they own, every car they are a viewer of, and
-- every device they sign in on. Keying it by car would mean somebody's home
-- address had to be re-entered per vehicle and would leak it onto a surface a
-- co-owner can read; keying it by device is exactly the bug being fixed.
--
-- PRIMARY KEY (user_id, kind) -- the natural key, and there is no surrogate
-- id. Same reasoning migration 0022 gives for its single-column key, one
-- dimension wider: a person has exactly ONE home and ONE work, so a surrogate
-- key would add a column that must then be constrained UNIQUE on
-- (user_id, kind) anyway, and would let a bug write a second 'home' row that
-- silently shadows the first. The upsert conflict target is the primary key
-- itself, which is what makes PUT an upsert in one statement.
--
-- TWO KINDS, CLOSED BY A CHECK:
--
--   home -- where the person lives
--   work -- where the person works
--
-- The CHECK keeps bugs out of the column exactly as the 0002 status CHECK
-- does. These are PEERS, not tiers: unlike the sharing permission ladder there
-- is no ordering here and neither kind implies the other. v1 deliberately
-- ships two FIXED slots rather than user-named favourites -- a named
-- collection would need ordering, renaming, a create endpoint and a per-person
-- cap, none of which this surface has, and none of which the two shortcut
-- chips need. Adding a third slot later is an additive contract enum member
-- plus a migration that widens this CHECK.
--
-- NO ROW MEANS NOT SET. There is no tombstone and no null-coordinate
-- placeholder: DELETE really deletes, and a kind the person has never set is
-- indistinguishable from one they removed. Both read as absent, which is the
-- only state the client can render anyway ("Set home"). This is why the wire
-- envelope is a SPARSE array of 0, 1 or 2 rows rather than a fixed pair with
-- nullable members -- a half-populated place is not a weaker place, it is not
-- a place, and modelling it would put a coordinate-less row in front of every
-- consumer for the entire life of the feature.
--
-- ENCRYPTION (NFR-3.23, docs/contracts/data-classification.md 1.17), and this
-- is the load-bearing part of the migration:
--
-- lat/lng are P1 GPS data stored ONLY as AES-256-GCM ciphertext in TEXT
-- columns (base64 [version][nonce][ct+tag]), following the go_ride_requests
-- precedent from migration 0002 EXACTLY -- the *_enc suffix, the same
-- cryptox.Encryptor key machinery, and the same encrypt-only posture. The
-- table is new, so there is no plaintext column, no dual-write rollout window
-- and no backfill: encrypt-only from day one, the way 0002 was and unlike the
-- MYR-63/64 shadow-column rollouts. The repo therefore requires an Encryptor
-- at construction (panics on nil) and treats a decrypt failure as a hard read
-- error, because there is no fallback column to fall back to.
--
-- WHY THE SAME TIER AS A RIDE PICKUP IS THE FLOOR AND NOT THE CEILING: a ride
-- coordinate is where somebody went once. A saved home coordinate is where
-- they SLEEP, it is durable, and it is re-read on every app launch. If
-- anything on this platform justifies column encryption it is this table, so
-- the precedent is followed to the letter rather than argued down.
--
-- The CIPHERTEXT IS NOT SORTABLE, COMPARABLE OR INDEXABLE, and nothing here
-- wants it to be. Every access is a point lookup by (user_id) or
-- (user_id, kind), which the primary key already serves; there is no
-- "places near me" query and no geospatial index, so encryption costs this
-- surface nothing it was using.
--
-- label is P1 too but is NOT app-level encrypted -- the same tier split 0002
-- makes between pickup_lat_enc and pickup_label, and the same rationale as the
-- reverse-geocoded strings in data-classification.md 3.2: a formatted address
-- string is meaningfully coarser than the coordinate pair it came from, the
-- pair beside it already carries the precision, and it is log-redacted either
-- way. Capped at 200 characters so the column is not an unbounded write; the
-- endpoint enforces the same cap on the RUNE count before it ever gets here.

CREATE TABLE IF NOT EXISTS go_saved_places (
    user_id    TEXT        NOT NULL,
    kind       TEXT        NOT NULL
        CONSTRAINT go_saved_places_kind_check CHECK (kind IN ('home', 'work')),

    -- P1 GPS, encrypt-only. NOT NULL: a place without coordinates is not a
    -- place, so the absent state is the absent ROW, never a NULL column.
    lat_enc    TEXT        NOT NULL,
    lng_enc    TEXT        NOT NULL,

    label      TEXT        NOT NULL
        CONSTRAINT go_saved_places_label_check CHECK (
            char_length(label) BETWEEN 1 AND 200
        ),

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT go_saved_places_pkey PRIMARY KEY (user_id, kind)
);

-- No secondary index. Every access -- the list read, the upsert, the delete
-- and the account-deletion sweep -- is a point lookup on the primary key or
-- its leading column, which the PK's own index already serves.
