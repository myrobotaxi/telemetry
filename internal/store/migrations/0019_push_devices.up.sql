-- 0019_push_devices.up.sql
--
-- MYR-186: go_push_devices — the APNs device-token registry. Push did not exist
-- before this migration: the app had unused notify* preference toggles and no
-- way to reach a phone that was not holding a live WebSocket. This table is the
-- missing address book — one row per (signed-in person, installed app instance)
-- — that the ride-lifecycle notification senders fan out over.
--
-- Naming convention (CG-DL-9): Go-owned table, "go_" prefix, snake_case
-- columns. The id is a Go-generated cuid (newProvisionID) matching the platform
-- convention, and user_id holds a cuid that identifies a row in the sibling
-- app's schema, but NO foreign key is declared: CG-DL-9 forbids Go migration
-- SQL from referencing tables owned by the sibling app's schema. Deleting a
-- person therefore leaves rows behind; the registry self-heals instead, because
-- APNs answers 410 Unregistered for a dead installation and the sender deletes
-- the row on that signal (see internal/push).
--
-- device_token carries a UNIQUE constraint because the token — not the id — is
-- the natural key: iOS mints one token per app installation, and that token can
-- legitimately move between people when a phone is handed over or a second
-- account signs in on the same device. The registration endpoint upserts on it
-- and RE-PARENTS the row to the caller, which is the only way to guarantee the
-- previous occupant stops receiving the new occupant's ride notifications.
--
-- sandbox records which APNs gateway minted the token. A token from a
-- development/TestFlight build is only valid against api.sandbox.push.apple.com
-- and a release-build token only against api.push.apple.com, so the flag is not
-- cosmetic: sending to the wrong host returns BadDeviceToken and would delete a
-- perfectly good row.
--
-- Classification: user_id, id, platform, sandbox and the two timestamps are P0
-- (opaque cuids, an enum, a bool, clock readings). device_token is P1: it is a
-- device identifier and a capability — anyone holding it plus the team's APNs
-- key can push to that phone. Stored raw and NOT app-level encrypted, for the
-- same reason as the §3.2 log-redaction-only fields: the sender needs the exact
-- bytes on every send, an encrypt/decrypt round-trip per push would buy nothing
-- against a database-read attacker who also holds APNS_KEY_P8, and the token is
-- useless without that key. Log-redaction is the control: only an 8-character
-- prefix is ever logged (push.tokenPrefix), and the value is never echoed into
-- an error envelope or a notification payload
-- (data-classification.md §1.14, §3.2).

CREATE TABLE IF NOT EXISTS go_push_devices (
    id            TEXT        PRIMARY KEY,
    user_id       TEXT        NOT NULL,
    device_token  TEXT        NOT NULL UNIQUE,
    platform      TEXT        NOT NULL DEFAULT 'ios'
        CONSTRAINT go_push_devices_platform_check CHECK (platform IN ('ios')),
    sandbox       BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The audience fan-out: every send resolves a ride party's cuid to their
-- devices, so this is the registry's hot read path.
CREATE INDEX IF NOT EXISTS idx_go_push_devices_user_id
    ON go_push_devices (user_id);
