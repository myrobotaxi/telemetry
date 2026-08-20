-- 0045_tesla_token_keepalive.up.sql
--
-- MYR-594: go_tesla_token_keepalive — the sweeper's memory of the keepalive arm.
-- ONE ROW PER OWNER, and it holds nothing but bookkeeping: when the arm last
-- tried this account, when it last worked, and when it last failed.
--
-- ── THE PROBLEM THIS SERVES ─────────────────────────────────────────────────
--
-- Tesla Fleet API refresh tokens are SINGLE-USE, ROTATING, and they LAPSE after
-- roughly three months of non-use. Every refresh the platform performs is
-- on-demand — something needed an access token, found it expired, and rotated.
-- MYR-592 then introduced the one population that generates no demand at all:
-- an owner whose vehicles are SUSPENDED makes zero Tesla calls, by design and
-- forever, until they come back. Three months of that and the refresh token
-- lapses, at which point MYR-592's one-tap reconnect degrades into a full OAuth
-- re-pair — the exact outcome the suspension design promised to avoid.
--
-- So a dormant owner's token is rotated deliberately, once, well before the
-- lapse window. The arm lives in internal/fleetsuspend (keepalive.go).
--
-- ── WHERE THE "LAST ROTATED" CLOCK LIVES, AND WHY IT IS NOT IN THIS TABLE ───
--
-- It is `expires_at` on the Prisma-owned OAuth row (provider = 'tesla'), a
-- BIGINT of Unix seconds, and it is already exactly the fact the arm needs.
-- EVERY writer of that row moves it: the Go on-demand refresh, the Go link and
-- re-link provisioning upsert, and the Next.js app's own refresh path. Since a
-- Tesla access token lives about eight hours, `expires_at` is the last rotation
-- instant plus a near-constant, so "rotated more than 45 days ago" reads
-- directly off it with no new state and, critically, NO WRITER TO ADD.
--
-- That is why this table does NOT carry a `last_rotated_at` column. A Go-side
-- copy of the rotation clock would be wrong the moment any of the other three
-- writers rotated a token, and two of them are in a different repository. The
-- authoritative clock stays where the rotation happens.
--
-- The consequence worth stating: on the day this deploys, the arm looks at the
-- REAL staleness of every dormant token rather than at a stamp it just wrote.
-- A token already 80 days old is refreshed on the first pass, which is the
-- whole point — a table that started everyone's clock at zero would sit on its
-- hands for 45 days while the tokens closest to lapsing lapsed. The burst that
-- design would have prevented is prevented instead by the arm's per-pass cap.
--
-- ── SO WHAT IS THIS TABLE FOR? THREE THINGS THE OAUTH ROW CANNOT SAY ────────
--
--   1. THE FAILURE COOLDOWN. A refresh Tesla REFUSES (lapsed, revoked, the user
--      pulled the grant in their Tesla account) must be logged once and then
--      LEFT ALONE. Without a record, every pass would re-ask forever: an hourly
--      signed call against Tesla's OAuth endpoint, per dead account, for the
--      life of the platform. `last_failure_at` is what makes the retry cost
--      one attempt a week instead.
--
--   2. THE ROTATION-LOOP BRAKE. The rotation is (a) call Tesla, (b) store the
--      new pair. If (b) fails, `expires_at` never moves, so the staleness gate
--      would select the same owner on the very next pass — and each retry burns
--      a single-use refresh token whose replacement we already failed to store.
--      `last_attempt_at` is written for FAILURES AND SUCCESSES ALIKE, so the arm
--      cannot spin on an account whose write path is broken.
--
--   3. THE OPERATOR'S ANSWER. `last_success_at` is the only durable record that
--      the arm ever worked for a given account; the pass log rotates away.
--
-- ── WHY THE COOLDOWN IS RELEASED BY THE TOKEN ROW, NOT ONLY BY TIME ─────────
--
-- `failed_token_expiry` stores the OAuth row's `expires_at` as observed at the
-- moment of the failure. A cooldown that were purely time-based would keep
-- ignoring an account for a week after the owner re-linked and handed us a
-- perfectly good token. Comparing the stored value against the row's current
-- one answers "has anything changed since we gave up?" without this table
-- having to be told; a re-link, a reconnect or any on-demand refresh moves
-- `expires_at` and releases the cooldown early. Time is the backstop, the token
-- row is the signal.
--
-- ── NO FK (CG-DL-9) ─────────────────────────────────────────────────────────
--
-- user_id is a plain TEXT column holding the cuid the OAuth row is keyed by,
-- exactly as go_user_activity (migration 0043) and go_profile_name_confirmations
-- (0042) are keyed. CG-DL-9 forbids a file in this directory from naming a
-- Prisma-owned table at all, so an FK is not merely undesirable but unwritable.
-- A row for a deleted account is inert: every reader arrives here through a join
-- from a live suspended vehicle.
--
-- ── CLASSIFICATION ──────────────────────────────────────────────────────────
--
-- P0 (docs/contracts/data-classification.md §1.23). An opaque cuid, three
-- timestamps about a platform action, and a copy of an expiry integer. NO TOKEN
-- MATERIAL IS STORED OR EVER MAY BE — the tokens live encrypted on the OAuth
-- row and are never copied here, not even in redacted form. `failed_token_expiry`
-- is an expiry SECOND, not a credential; it reveals when a grant was last
-- rotated and nothing about its value. Nothing in this table reaches any wire.

CREATE TABLE IF NOT EXISTS go_tesla_token_keepalive (
    -- The dormant owner. Opaque cuid, no FK (CG-DL-9). The same subject the
    -- OAuth row, go_user_activity and every other Go-owned table file this
    -- person under.
    --
    -- PRIMARY KEY rather than a surrogate id: an owner has one keepalive
    -- history, and a second row would not be a second fact. It also makes the
    -- recorder a point upsert and gives the candidate query's LEFT JOIN an
    -- index for free.
    user_id             TEXT PRIMARY KEY,

    -- The last time the arm TRIED this owner, whatever came of it. NOT NULL,
    -- because the row is created by an attempt and there is no state a NULL
    -- could describe. This is the rotation-loop brake: it advances even when
    -- storing the rotated pair failed, which is the one case where the
    -- authoritative clock does not move.
    last_attempt_at     TIMESTAMPTZ NOT NULL,

    -- The last attempt that rotated and stored a pair. NULL means the arm has
    -- never succeeded for this owner — a brand-new row, or an account that has
    -- only ever refused.
    last_success_at     TIMESTAMPTZ,

    -- The last attempt Tesla (or the store) refused. NULL means no failure is
    -- outstanding. Non-NULL starts the cooldown; the arm skips the owner until
    -- it ages out or the token row moves underneath it.
    last_failure_at     TIMESTAMPTZ,

    -- The OAuth row's expiry second as observed at that failure, or NULL when
    -- there is no outstanding failure. Compared against the row's CURRENT value
    -- to release the cooldown early — see the header. Deliberately nullable and
    -- deliberately not an expiry we compute: it is a fingerprint of the token
    -- generation we failed on, and its only use is inequality.
    failed_token_expiry BIGINT
);

-- One partial index, on the rows the arm can act on. The candidate query drives
-- from suspended vehicles and LEFT JOINs here by user_id (the primary key), so
-- this index is not for the join — it is for keeping the table's own scan cheap
-- as the cooled-down failures accumulate, which is the one class of row here
-- that is never deleted and never stops matching.
CREATE INDEX IF NOT EXISTS idx_go_tesla_token_keepalive_attempted
    ON go_tesla_token_keepalive (last_attempt_at);
