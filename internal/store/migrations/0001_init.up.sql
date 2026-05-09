-- 0001_init.up.sql
--
-- Bootstraps the golang-migrate schema_migrations tracking table and
-- proves the Go-owned migration toolchain is wired correctly.
--
-- This is a PLACEHOLDER migration. No application feature depends on
-- _telemetry_server_meta; it exists solely to validate that:
--   (a) the migrate runner can connect and apply SQL,
--   (b) the embed.FS source loads correctly at binary startup,
--   (c) running migrations twice produces ErrNoChange (idempotent).
--
-- Naming convention (enforced by CG-DL-9):
--   All Go-owned tables MUST be prefixed "_telemetry_" or "go_" so that
--   "prisma db pull" output can be filtered without touching Prisma-owned
--   tables (User, Account, Vehicle, Drive, TripStop, Invite, Settings,
--   AuditLog). See docs/architecture/migrations.md for the coexistence rule.

CREATE TABLE IF NOT EXISTS _telemetry_server_meta (
    key                TEXT PRIMARY KEY,
    value              TEXT        NOT NULL,
    bootstrapped_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO _telemetry_server_meta (key, value)
VALUES ('schema_version', '1')
ON CONFLICT (key) DO NOTHING;
