-- 0020_vehicle_shares.up.sql
--
-- MYR-184: go_vehicle_shares — the vehicle-sharing grant table. Before this
-- migration an owner had no way to let anyone else see their car: the only
-- access set the server knew was "vehicles whose userId is you", and the
-- `viewer` role existed in the mask matrix without a single row that could
-- ever produce it. This table is that missing row. One record is ONE
-- owner→recipient grant for ONE vehicle, in either of its two live shapes —
-- an unredeemed invite carrying a redeemable `code`, or an accepted grant
-- naming the person who redeemed it.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case
-- columns. `id` is a Go-generated cuid (newProvisionID), and vehicle_id /
-- owner_user_id / accepted_by_user_id all hold cuids that identify rows in
-- the sibling app's schema — but NO foreign key is declared and no
-- sibling-owned relation is named anywhere in this file, because CG-DL-9
-- forbids Go migration SQL from referencing tables owned by that schema.
-- Deleting a car or a person therefore leaves rows behind; they are inert
-- (the read paths resolve the target through the sibling schema and simply
-- find nothing), and the owner-teardown path revokes them explicitly.
--
-- CODES, NOT EMAILS. MyRoboTaxi has no email infrastructure, so the owner
-- hands out a 6-character code out-of-band through the iOS share sheet. One
-- code may back N rows — a multi-vehicle invite is N rows sharing a single
-- code, redeemed atomically — which is exactly why `code` is NOT unique here
-- and why the pending-code index below is a plain partial index.
--
-- REVOCATION IS A TOMBSTONE, NOT A DELETE. status flips to 'revoked' and
-- revoked_at is stamped; the row stays for audit. Nothing outside the server
-- ever sees a revoked row — the wire status enum has only 'pending' and
-- 'accepted', and the list query filters revoked rows out.
--
-- EXPIRY IS NOT A STATUS. An expired invite stays 'pending' with an
-- expires_at in the past and simply stops redeeming (the redeem statement
-- carries `expires_at > NOW()` in its WHERE clause). There is no sweeper: a
-- dead code is dead because the redeem predicate says so, checked at the only
-- moment it matters.
--
-- Classification (data-classification.md §1.15): id / vehicle_id /
-- owner_user_id / accepted_by_user_id / permission / status and the four
-- timestamps are P0 — opaque cuids, an authorization tier, a lifecycle enum,
-- clock readings. TWO columns are P1. `label` is a person's name, typed by
-- the owner for their own list ("Mira Chen", "Mom"): log-redacted, never
-- delivered to anyone but the owner. `code` is a live BEARER CREDENTIAL for
-- vehicle access — anyone holding it can redeem it — and is handled like one:
-- NEVER logged (log the invite id instead), never echoed into an error
-- envelope, never returned on a non-owner surface, and omitted from the wire
-- the moment the row leaves 'pending'. It is stored raw and not app-level
-- encrypted for the same reason as the §3.2 log-redaction-only fields: the
-- redeem lookup matches on the exact bytes, and the value is short-lived
-- (7 days) and single-purpose.

CREATE TABLE IF NOT EXISTS go_vehicle_shares (
    id                  TEXT        PRIMARY KEY,
    vehicle_id          TEXT        NOT NULL,
    owner_user_id       TEXT        NOT NULL,
    label               TEXT        NOT NULL,
    permission          TEXT        NOT NULL
        CONSTRAINT go_vehicle_shares_permission_check
        CHECK (permission IN ('live', 'live_history', 'rides')),
    code                TEXT        NOT NULL,
    status              TEXT        NOT NULL DEFAULT 'pending'
        CONSTRAINT go_vehicle_shares_status_check
        CHECK (status IN ('pending', 'accepted', 'revoked')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at          TIMESTAMPTZ NOT NULL,
    accepted_at         TIMESTAMPTZ,
    accepted_by_user_id TEXT,
    revoked_at          TIMESTAMPTZ
);

-- The owner's sharing screen: every live row for one car, newest first.
-- Partial on "not revoked" because tombstones are never listed and the
-- table's revoked tail grows without bound.
CREATE INDEX IF NOT EXISTS idx_go_vehicle_shares_vehicle_live
    ON go_vehicle_shares (vehicle_id)
    WHERE status <> 'revoked';

-- The redeem lookup. Partial on 'pending' because a code is only ever
-- matched while it can still be redeemed, and it keeps the index to the
-- small live set rather than every code ever minted. NOT unique: a
-- multi-vehicle invite deliberately shares one code across N rows.
CREATE INDEX IF NOT EXISTS idx_go_vehicle_shares_code_pending
    ON go_vehicle_shares (code)
    WHERE status = 'pending';

-- One accepted grant per (person, vehicle) — the constraint that makes
-- redeem idempotent instead of duplicating grants on a retried request, and
-- that stops two invites for the same car stacking on one viewer.
--
-- Column order is (accepted_by_user_id, vehicle_id) rather than the reverse:
-- uniqueness over a pair is order-independent, but the leading column is
-- not. This ordering makes the SAME index serve the viewer access-set
-- lookup — "which vehicles has this person been granted?", run on every
-- WebSocket handshake and every GetUserVehicles miss — instead of needing a
-- fourth index for it. The per-vehicle direction is already covered by
-- idx_go_vehicle_shares_vehicle_live above.
CREATE UNIQUE INDEX IF NOT EXISTS uq_go_vehicle_shares_accepted_grant
    ON go_vehicle_shares (accepted_by_user_id, vehicle_id)
    WHERE status = 'accepted';
