-- 0003_identity.up.sql
--
-- MYR-193: the identity module's own tables (ADR-001,
-- docs/architecture/adr-001-identity-module.md). Native Sign in with Apple
-- resolves to a user CUID; ES256 access tokens + rotating refresh tokens are
-- minted by internal/identity.
--
-- Naming convention (CG-DL-9): all three are Go-owned, "go_" prefix, and
-- reference user cuids WITHOUT foreign keys — CG-DL-9 forbids Go migration
-- SQL from referencing tables owned by the Next.js app's Prisma schema (the
-- linkage to the Prisma "User" row is resolved at RUNTIME by a read-only
-- SELECT in internal/identity, never by a DB constraint here).
--
-- Classification (docs/contracts/data-classification.md §1.10–§1.12): email
-- and name are P1 (PII, never logged); apple_sub / user_id / family_id /
-- token_hash are P0 opaque identifiers; the raw refresh token is NEVER
-- stored — only its SHA-256 hex digest (token_hash).

-- go_users: Apple-native users with no legacy Prisma "User" row. A user who
-- signed up through the old web app already has a Prisma "User" cuid and is
-- linked by email-match instead (their id lives in "User", not here). Only
-- genuinely-new Apple users get a row here. The validator's user-existence
-- check accepts a CUID present in EITHER "User" OR go_users.
CREATE TABLE IF NOT EXISTS go_users (
    id          TEXT PRIMARY KEY,             -- cuid-shaped, Go-generated
    email       TEXT,                          -- P1; verified email at creation (nullable: Apple hidden-email)
    name        TEXT,                          -- P1; display name from first sign-in (nullable)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Partial unique index on email: at most one go_users row per non-null email,
-- so a repeat mint can't fork a second Apple-native account for one address.
CREATE UNIQUE INDEX IF NOT EXISTS idx_go_users_email
    ON go_users (email) WHERE email IS NOT NULL;

-- go_identity_apple: the authoritative apple_sub -> user_id binding. Written
-- once on first sign-in (LINKAGE precedence in ADR-001 §4); reused verbatim
-- afterwards so a later email change can never re-point an Apple account.
CREATE TABLE IF NOT EXISTS go_identity_apple (
    apple_sub     TEXT PRIMARY KEY,           -- Apple's stable subject ("sub" claim)
    user_id       TEXT NOT NULL,              -- resolved CUID: a Prisma "User".id OR a go_users.id (no FK, CG-DL-9)
    email         TEXT,                        -- P1; email captured at first sign-in (nullable)
    name          TEXT,                        -- P1; name captured at first sign-in (nullable)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Reverse lookup: all Apple identities bound to a user (rare, but keeps the
-- user_id -> apple_sub direction indexed for ops/debug).
CREATE INDEX IF NOT EXISTS idx_go_identity_apple_user
    ON go_identity_apple (user_id);

-- go_refresh_tokens: hash-only refresh-token store with single-use rotation
-- and family reuse-detection (ADR-001 §5). The raw token is never persisted.
CREATE TABLE IF NOT EXISTS go_refresh_tokens (
    token_hash   TEXT PRIMARY KEY,            -- SHA-256 hex of the opaque refresh token
    family_id    TEXT NOT NULL,               -- rotation lineage; reuse-detection revokes the whole family
    user_id      TEXT NOT NULL,               -- owning user cuid (no FK, CG-DL-9)
    issued_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL,        -- sliding 90d: each rotation sets now + 90d
    rotated_from TEXT,                          -- token_hash this one replaced (NULL for the family root)
    rotated_to   TEXT,                          -- token_hash that replaced this one (set on rotation; non-NULL => spent)
    revoked      BOOLEAN NOT NULL DEFAULT FALSE,
    revoked_at   TIMESTAMPTZ,
    reason       TEXT                           -- 'rotated' | 'revoked' | 'reuse_detected' (opaque enum, P0)
);

-- Family revocation touches every row in a family_id.
CREATE INDEX IF NOT EXISTS idx_go_refresh_tokens_family
    ON go_refresh_tokens (family_id);

-- Per-user session enumeration + the background expiry sweep.
CREATE INDEX IF NOT EXISTS idx_go_refresh_tokens_user
    ON go_refresh_tokens (user_id);
CREATE INDEX IF NOT EXISTS idx_go_refresh_tokens_expires
    ON go_refresh_tokens (expires_at);
