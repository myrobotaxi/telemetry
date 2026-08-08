package store

// Every SQL statement OwnerProvisioner issues, in one file so the whole
// identity-resolution statement set can be read at once — the same arrangement
// account_deletion_queries.go uses for the teardown, and for the same reason:
// each WHERE and each ON CONFLICT arm below is a decision about whose account a
// Tesla link resolves to, and those read better together than scattered through
// the control flow that invokes them.
//
// Split out of owner_provision.go, which had grown past the 300-line file cap.
// The session-migration statements MYR-488 added are the exception and live in
// owner_session_migration.go, next to the concurrency argument that explains
// their shape.

const queryProvisionUser = `
INSERT INTO "User" ("id", "name", "email", "updatedAt")
VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NOW())
ON CONFLICT ("id") DO NOTHING`

const queryFindTeslaAccountUser = `
SELECT "userId" FROM "Account"
WHERE "provider" = 'tesla' AND "providerAccountId" = $1
LIMIT 1`

const queryFindUserByEmail = `
SELECT "id" FROM "User" WHERE lower("email") = lower($1) LIMIT 1`

const queryRebindAppleIdentity = `
UPDATE go_identity_apple SET user_id = $1 WHERE user_id = $2`

// queryRecordConvergence leaves the trail the re-point above would otherwise
// sever (MYR-452). After rebindApple, nothing references the caller's id: the
// binding now names the canonical one, so the caller's id is reachable from
// nowhere — while the caller's JWT keeps carrying it. Account deletion is keyed
// on that subject, so without this row the teardown would target an id that
// owns nothing and leave the real account (binding included) standing.
//
// It writes to go_identity_convergence rather than to a column on go_users
// because `from` is whatever the caller's token names, which is frequently not
// a go_users id at all — see the table's migration comment.
//
// No attempt is made here to keep the graph flat. Chains and cycles are both
// reachable (re-linking the same Tesla under the new canonical id fires a
// convergence back the other way), so the resolver walks the graph with a
// visited set instead of relying on an invariant this statement cannot hold.
// The conflict arm deliberately REFUSES to re-target an existing edge: it
// refreshes the timestamp only when the target is identical, and otherwise
// leaves the stored edge alone. Silently repointing would move a live delete
// grant from one account to another. With the RowsAffected guard in rebindApple
// a divergent conflict should be unreachable, so this is the second lock on the
// same door.
const queryRecordConvergence = `
INSERT INTO go_identity_convergence (from_user_id, to_user_id)
VALUES ($2, $1)
ON CONFLICT (from_user_id) DO UPDATE
SET converged_at = NOW()
WHERE go_identity_convergence.to_user_id = EXCLUDED.to_user_id`

const queryProvisionSettings = `
INSERT INTO "Settings" ("id", "userId", "teslaLinked", "updatedAt")
VALUES ($1, $2, TRUE, NOW())
ON CONFLICT ("userId") DO UPDATE
SET "teslaLinked" = TRUE, "updatedAt" = NOW()`

// queryProvisionAccount upserts the owner's Tesla OAuth tokens as
// ciphertext only (MYR-433). The plaintext "access_token" /
// "refresh_token" columns are never populated by this server.
//
// The DO UPDATE branch actively NULLs them. That is deliberate and is the
// one place this server can scrub on the write path: re-linking a Tesla
// account is exactly when a stale plaintext token from the pre-MYR-433
// dual-write era would otherwise be refreshed and live on. Setting them
// to NULL here means a re-link cleans the row instead of preserving a
// credential the ciphertext column already holds.
//
// #nosec G101 -- column-name SQL, not a credential (gosec greps the literal for
// access_token/refresh_token and misflags it as a hardcoded secret).
const queryProvisionAccount = `
INSERT INTO "Account" (
    "id", "userId", "type", "provider", "providerAccountId",
    "access_token_enc",
    "refresh_token_enc",
    "expires_at"
) VALUES ($1, $2, 'oauth', 'tesla', $3, $4, $5, $6)
ON CONFLICT ("provider", "providerAccountId") DO UPDATE
SET "access_token_enc"  = EXCLUDED."access_token_enc",
    "refresh_token_enc" = EXCLUDED."refresh_token_enc",
    "expires_at"        = EXCLUDED."expires_at",
    "access_token"      = NULL,
    "refresh_token"     = NULL`
