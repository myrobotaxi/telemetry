-- 0030_fleet_config_attempts.up.sql
--
-- MYR-448: go_fleet_config_attempts — per-vehicle scheduling state for the
-- fleet-config reconciler.
--
-- WHY THIS TABLE EXISTS AT ALL. The reconciler's job is to re-push a
-- fleet-telemetry config to cars that were provisioned but never configured
-- (Tesla skips the link-time push, because the owner cannot have paired the
-- virtual key during the OAuth callback). The obvious candidate ordering is
-- `ORDER BY "Vehicle"."lastUpdated" ASC LIMIT n` — and it is WRONG, in a way
-- that silently defeats the entire feature.
--
-- Nothing the reconciler does writes `"Vehicle"."lastUpdated"`; only the
-- streaming pipeline does. So for any car the reconciler CANNOT fix — an owner
-- who links and never pairs, an abandoned-but-configured car, a VIN Tesla
-- 403s — `lastUpdated` is frozen and ages monotonically, which means it sorts
-- FIRST forever. Twenty such cars (trivially reachable in a beta) permanently
-- occupy the whole `LIMIT`, and owner twenty-one is never examined again. The
-- logs would read "pass complete, examined 20" and look perfectly healthy
-- while the reconciler healed nobody.
--
-- The fix is to order by OUR OWN attempt schedule rather than by a column we
-- do not control. `next_attempt_at` advances every time we touch a car, so the
-- cursor always moves and coverage is guaranteed.
--
-- IT IS ALSO THE BACKOFF. `AwaitingKey` is the expected steady state for an
-- owner who has not paired yet, and at a flat 15-minute cadence that is ~96
-- signed POSTs per car per day against Tesla's rate-limited signing path,
-- forever. attempt_count drives exponential backoff (15m → 30m → 1h → … capped
-- at 12h) so a car that cannot be fixed costs progressively less, while a car
-- that CAN be fixed is still reached promptly after the owner pairs.
--
-- LIFECYCLE. A row appears on the first attempt against a vehicle and is
-- DELETED the moment the push succeeds — success needs no schedule, and the
-- car will not be a candidate again unless it goes quiet, at which point it
-- starts from a clean count. Rows are therefore bounded by "cars currently
-- failing to stream", not by lifetime vehicle volume, so the table is
-- self-draining in the same way 0016/0026/0028 are.
--
-- Not wire-exposed. Keyed by the Prisma "Vehicle"."id" (TEXT cuid), with no FK:
-- CG-DL-9 forbids a Go-owned table constraining a Prisma-owned one, and a
-- stale row for a deleted vehicle is harmless — it simply never matches the
-- candidate join again. (The reconciler prunes on success; a deleted car's row
-- is swept by the same DELETE path when the vehicle is torn down.)
--
-- Classification: vehicle_id is P0 (opaque cuid). No VIN, no token, no
-- coordinate, no PII. last_outcome is an internal enum-ish label
-- ('awaiting_virtual_key', 'skipped_other', 'read_failed', 'push_failed',
-- 'token_failed'), never a raw Tesla body. Naming: Go-owned table, "go_"
-- prefix, snake_case.

CREATE TABLE IF NOT EXISTS go_fleet_config_attempts (
    vehicle_id      TEXT        PRIMARY KEY,
    attempt_count   INTEGER     NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_outcome    TEXT        NOT NULL DEFAULT ''
);

-- Serves the candidate query's `next_attempt_at <= NOW()` filter and its
-- `ORDER BY next_attempt_at` in one ordered scan, so the LIMIT short-circuits
-- instead of sorting every backed-off row.
CREATE INDEX IF NOT EXISTS idx_go_fleet_config_attempts_due
    ON go_fleet_config_attempts (next_attempt_at);
