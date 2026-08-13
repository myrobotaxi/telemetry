-- 0040_ride_group.up.sql
--
-- MYR-540: GROUP RIDES — a ride other people may join, through a signed link
-- the server mints when the owner accepts (contracts v0.35.0
-- RideRequest.groupRide / .shareUrl / .members, $defs.RideMember,
-- $defs.RideRequestJoinRequest; CLIENT-DIRECTED: app + sign-in, full rider
-- view for joiners, full collaboration, no cap).
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case, and
-- the one foreign key points at another GO-OWNED table (go_ride_requests).
-- Nothing here names a Prisma-owned relation.
--
-- THREE COLUMNS ON THE RIDE ROW, ONE NEW TABLE, and the split is the same one
-- MYR-539 made for stops: a fact that is at most ONE per ride is a column, a
-- set of unbounded size is rows.
--
--   * group_ride           — the create-time flag. NOT NULL DEFAULT FALSE, so
--                            every row written before today reads as an
--                            ordinary solo ride, which is exactly what the
--                            contract's "absent means false" says.
--   * join_code            — the 6-character join code, minted AT ACCEPT.
--   * join_code_expires_at — the code's expiry, computed on the DATABASE clock
--                            at mint time and read back by the redeem
--                            predicate on the same clock.
--
-- WHY COLUMNS RATHER THAN A go_ride_share_links TABLE. This mirrors the
-- storage idiom the MYR-368 invite link already uses: go_vehicle_shares
-- carries `code` and `expires_at` on the row the code grants access TO, and
-- the redeem statement is one guarded UPDATE/SELECT against that row. A join
-- code grants membership of exactly ONE ride and there is at most one live
-- code per ride, so a side table would be a one-to-at-most-one join bought at
-- the price of a second write inside the accept transaction.
--
-- THE CODE SPACE IS SEPARATE AND SINGLE-PURPOSE, and this is where that is
-- enforced rather than merely intended: an invite code lives in
-- go_vehicle_shares.code and a join code lives here, so POST /api/invites/redeem
-- cannot resolve a join code and POST /api/ride-requests/join cannot resolve an
-- invite code — each statement reads one column of one table. Identical
-- alphabet (^[A-Z0-9]{6}$), identical length, disjoint namespaces.
--
-- P1 AND BEARER. join_code is a live bearer credential for a ride's live
-- location (data-classification.md §1.9, the same tier as ShareInvite.code):
-- never logged, never echoed into an error envelope, delivered only on the
-- party-scoped REST surfaces. It is stored in plaintext for the same reason
-- the invite code is — the redeem lookup is a by-code equality probe, and a
-- digest column would make that probe impossible without also making the
-- rate limit the only defence.
--
-- IF NOT EXISTS throughout, the 0034/0036/0037/0038/0039 convention:
-- TestMigration0004 re-applies the ladder over an existing schema.

ALTER TABLE go_ride_requests
    ADD COLUMN IF NOT EXISTS group_ride           BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS join_code            TEXT,
    ADD COLUMN IF NOT EXISTS join_code_expires_at TIMESTAMPTZ;

-- THE LOOKUP PATH AND THE UNIQUENESS GUARANTEE IN ONE INDEX.
--
-- PARTIAL on `join_code IS NOT NULL`, which is what makes uniqueness possible
-- at all: every solo ride and every un-accepted group ride carries NULL here,
-- and a plain UNIQUE index would be satisfied by those (NULLs never collide)
-- but would also index every row in the table for nothing. The partial index
-- covers only the rides that actually have a live code.
--
-- The uniqueness is LOAD-BEARING, not tidiness: the redeem statement resolves a
-- code to a ride with a bare equality predicate, so two rides sharing a code
-- would make redemption ambiguous — and the mint retries on the 23505 this
-- index raises, which is the whole collision story.
CREATE UNIQUE INDEX IF NOT EXISTS uq_go_ride_requests_join_code
    ON go_ride_requests (join_code)
    WHERE join_code IS NOT NULL;

-- go_ride_members — one row per person who JOINED a group ride by redeeming
-- its link. The REQUESTER IS NEVER A ROW HERE (they are go_ride_requests.rider_id)
-- and neither is the owner: the contract's `members` array is joiners only, which
-- is what keeps every pre-MYR-540 ride byte-identical on the wire.
--
-- THE COMPOSITE PRIMARY KEY IS THE IDEMPOTENCE. (ride_id, user_id) unique means
-- a re-redeemed code inserts nothing on the second attempt — the join handler's
-- INSERT ... ON CONFLICT DO NOTHING leans on this key, so "join twice" is one
-- membership and one 200, arbitrated by the database rather than by a
-- check-then-write in Go.
--
-- ON DELETE CASCADE for the same reason go_ride_stops has it: the MYR-447
-- retention sweep deletes terminal ride rows directly, and a membership row that
-- outlived its ride would be a standing grant on a vehicle with nothing left to
-- revoke it. Membership is RIDE-SCOPED and dies with the ride — the cascade is
-- what makes that a schema fact instead of a promise.
--
-- NO FOREIGN KEY ON user_id, deliberately and per CG-DL-9: user identity lives
-- across three relations (the Prisma-owned "User", go_identity_apple, go_users)
-- and one of them is not ours to reference. The account-deletion sweep
-- (0040's companion change in account_deletion_queries.go) deletes these rows
-- explicitly instead.
--
-- The row carries NO NAME. A member's first name is DERIVED on read through the
-- same three-source identity ladder requesterName uses, so a member who changes
-- their name does not leave a stale copy behind in a ride's history, and this
-- table stores no PII at all.
CREATE TABLE IF NOT EXISTS go_ride_members (
    ride_id   TEXT        NOT NULL
        REFERENCES go_ride_requests (id) ON DELETE CASCADE,
    user_id   TEXT        NOT NULL,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT pk_go_ride_members PRIMARY KEY (ride_id, user_id)
);

-- "Which rides is this person a member of?" — the access-set leg (the WS
-- handshake's vehicle set and the rider list's widening) and the join handler's
-- 409 probe both ask it. The primary key is (ride_id, user_id) and therefore
-- useless for a user_id-leading scan, so this index is the one that serves it.
CREATE INDEX IF NOT EXISTS idx_go_ride_members_user
    ON go_ride_members (user_id);
