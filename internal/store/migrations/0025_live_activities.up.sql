-- 0025_live_activities.up.sql
--
-- MYR-172: go_live_activities — the ActivityKit push-token registry behind the
-- rider's Live Activity for an active ride. Migration 0019 gave us an address
-- book of PHONES (go_push_devices, one row per installed app instance). This
-- table is an address book of LIVE ACTIVITIES, which is a different thing with
-- a different lifetime, and the distinction is the whole reason it is not two
-- more columns on 0019.
--
-- WHY A SEPARATE TABLE AND NOT A COLUMN ON go_push_devices.
--
-- An APNs device token addresses an INSTALLATION and lives as long as the app
-- is installed. An ActivityKit update token addresses ONE RUNNING ACTIVITY and
-- lives for the length of a single ride — minutes. They are minted by different
-- APIs, they are sent to different APNs topics ({bundleId} versus
-- {bundleId}.push-type.liveactivity), and one phone can legitimately hold both
-- at once. Hanging an activity token off the device row would mean a column
-- that is NULL almost always, that has no honest place to record WHICH ride it
-- belongs to, and that a second concurrent ride would overwrite. The row here
-- is keyed by the thing that actually owns the token's lifetime: the ride.
--
-- THE NATURAL KEY IS (ride_request_id, user_id), and it is a UNIQUE constraint
-- rather than a composite primary key so the row keeps an opaque `id` in the
-- shape every other Go-owned table uses (a Go-generated cuid, newProvisionID).
--
-- Why that pair and not the token: ActivityKit ROTATES the update token during
-- the life of a single Activity — the system hands the app a new one and
-- expects the server to start using it — so the token is emphatically NOT a
-- stable identity the way a device token is. Registration is therefore an
-- UPSERT on (ride_request_id, user_id) that REPLACES activity_push_token, and a
-- rotation is an ordinary re-registration that leaves one row where there was
-- one row. Making the token unique instead (the 0019 posture) would accumulate
-- a row per rotation and leave us guessing which one is live.
--
-- user_id is in the key, not merely along for the ride, because the same ride
-- has two parties. v1 only ever starts a rider-side Activity, but the owner
-- variant is an explicit MYR-172 follow-up, and a key that admitted only one
-- row per ride would have to be migrated the day that lands. It also makes the
-- rider-only authorization check expressible as part of the write.
--
-- THE FOREIGN KEY — the first one in the go_ namespace, deliberately.
--
-- No previous Go-owned migration declares a REFERENCES clause, and that is not
-- an accident: CG-DL-9 forbids this SQL from referencing the sibling app's
-- Prisma-owned tables, so every cuid pointing at a person or a car (user_id
-- here, and every user_id/vehicle_id in 0002 and 0019) is an unenforced
-- pointer that self-heals instead. go_ride_requests is different in kind: it is
-- OURS, it is created by migration 0002 under the same namespace, and CG-DL-9
-- has nothing to say about it.
--
-- The FK earns its place because this row is meaningless without its ride and
-- because the ride already has a hard-delete path: owner teardown issues a bare
-- DELETE over go_ride_requests by vehicle, and account deletion cascades. ON
-- DELETE CASCADE means those paths cannot strand a token that addresses an
-- Activity for a ride that no longer exists. The alternative — an unenforced
-- pointer plus a bespoke delete in every teardown path — is three places to
-- forget instead of zero. user_id stays unenforced, as it must.
--
-- ended_at IS A TOMBSTONE, NOT A DELETE.
--
-- A terminal ride status sends one last update carrying `event: "end"` and a
-- dismissal-date, then stamps ended_at. The row is kept, not removed, for two
-- reasons. First, the end update is not guaranteed to be the last write: the
-- ride-status event that produced it can be republished (see the at-most-once
-- caveat on push.Notifier), and a row that had been deleted would be silently
-- re-created by a late registration, leaving a token nobody will ever end.
-- Second, ended_at IS NULL is the exact predicate the ETA ticker and the
-- lifecycle fan-out select on, so ending an Activity is one UPDATE that removes
-- it from every send path at once, with no cross-table coordination.
--
-- Rows are swept 24 hours after their last write (see the sweeper in
-- internal/push), which covers both the ordinary ended row and the row whose
-- Activity died on the phone without ever telling us — the case a delete-on-end
-- design has no answer for at all.
--
-- NULLABILITY. activity_push_token is NOT NULL because a row without a token
-- addresses nothing and there is no honest-unknown state; a caller with no
-- token simply does not register. ended_at is NULLABLE and NULL is the ordinary
-- active state, so a freshly upserted row is correct untouched — the same
-- posture as suspended_at in 0024, and for the same reason: it records WHEN,
-- which a boolean cannot, and "when did this Activity stop being updated?" is
-- the first question asked when a rider reports a stuck lock screen.
--
-- sandbox mirrors go_push_devices.sandbox and is NOT cosmetic: a token minted
-- by a development/TestFlight build is only valid against
-- api.sandbox.push.apple.com. It is carried per-activity rather than joined
-- from the device registry because the Activity is started by a specific build
-- and the device row for that person may not exist at all — a rider who
-- declined the notification permission still gets a Live Activity, which
-- requires no permission prompt.
--
-- Classification (data-classification.md §1.18): id, ride_request_id, user_id,
-- sandbox and the three timestamps are P0 (opaque cuids, a build-flavour flag,
-- clock readings). activity_push_token is P1, the same tier and the same
-- handling rule as go_push_devices.device_token: a capability that lets whoever
-- holds it — together with APNS_KEY_P8, an environment secret and not a column
-- — write to the lock screen of that phone. Stored raw and NOT app-level
-- encrypted for the §3.2 reason: the sender needs the exact bytes on every
-- send, so a round-trip per push would buy nothing against a database-read
-- attacker who also holds the signing key. Log redaction is the control — only
-- an 8-character prefix is ever logged (push.tokenPrefix) — and the value is
-- never echoed into a response or an error envelope.

CREATE TABLE IF NOT EXISTS go_live_activities (
    id                   TEXT        PRIMARY KEY,
    ride_request_id      TEXT        NOT NULL
        REFERENCES go_ride_requests (id) ON DELETE CASCADE,
    user_id              TEXT        NOT NULL,
    activity_push_token  TEXT        NOT NULL,
    sandbox              BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at             TIMESTAMPTZ,

    -- The upsert conflict target AND the fan-out read path. One Activity per
    -- (ride, party): a rotated token replaces the value in place.
    CONSTRAINT go_live_activities_ride_user_key UNIQUE (ride_request_id, user_id)
);

-- The 24-hour sweep's predicate. Deliberately on updated_at rather than
-- ended_at: the rows most worth reaping are the ones that NEVER ended (the
-- Activity died on the phone), and those have ended_at IS NULL forever.
CREATE INDEX IF NOT EXISTS idx_go_live_activities_updated_at
    ON go_live_activities (updated_at);

-- The ETA ticker sweeps every still-live Activity across all rides on each
-- tick, so its predicate is `ended_at IS NULL` with no ride in hand. A partial
-- index over the live rows keeps that scan proportional to the number of rides
-- actually in flight rather than to every ride ever taken.
CREATE INDEX IF NOT EXISTS idx_go_live_activities_live
    ON go_live_activities (ride_request_id)
    WHERE ended_at IS NULL;
