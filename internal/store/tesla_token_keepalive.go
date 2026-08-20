// Package store — the candidate read and the bookkeeping writes for MYR-594's
// dormant-owner token keepalive: go_tesla_token_keepalive (migration 0045).
//
// ── THE ROTATION CLOCK IS NOT IN THE GO-OWNED TABLE ─────────────────────────
//
// It is `expires_at` on the Prisma-owned OAuth row, and the candidate query
// below reads it directly. Every writer of that row moves it — this server's
// on-demand refresh, this server's link/re-link provisioning upsert, and the
// sibling Next.js app's own refresh — so it is the only value in the schema
// that answers "when was this grant last rotated?" for all three. A Tesla access
// token lives about eight hours, so the answer is `expires_at` minus a constant
// small enough to disappear against a 45-day gate.
//
// A Go-side copy would be wrong the moment either of the other two writers
// rotated, and two of them live in a different repository. Migration 0045's
// header argues this at length; the short version is that the authoritative
// clock stays where the rotation happens, and go_tesla_token_keepalive holds
// only what the OAuth row cannot say: the failure cooldown, the rotation-loop
// brake, and whether the arm has ever worked.
//
// P0 in full (data-classification.md §1.23): opaque cuids, timestamps and an
// expiry second. No token material is read into this file's results and none is
// ever written to the Go-owned table.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StaleTokenOwner is one dormant owner whose Tesla grant is going stale: their
// cars are suspended, they have been silent for days, and their OAuth row has
// not been rotated inside the keepalive window.
type StaleTokenOwner struct {
	// OwnerID is the cuid the OAuth row and go_user_activity are both keyed by.
	OwnerID string
	// TokenExpiresAt is the OAuth row's `expires_at` as a time — the rotation
	// clock the staleness gate fired on. Carried out so the arm can log how
	// stale the grant was and so a failure can be pinned to this exact
	// generation of the token (see RecordKeepaliveFailure).
	TokenExpiresAt time.Time
	// TokenExpiryUnix is the same value verbatim from the column, kept because
	// the cooldown release compares it for INEQUALITY against whatever the row
	// says later. Comparing the raw integer cannot disagree with the database
	// about rounding or zone the way a round-tripped timestamp could.
	TokenExpiryUnix int64
}

// StaleTokenOwnerQuery is the four thresholds and the cap. A struct rather than
// five positional arguments because four of the five are timestamps and a
// transposed pair would silently change the policy rather than fail to compile.
type StaleTokenOwnerQuery struct {
	// StaleBefore: the grant must not have been rotated since this instant.
	StaleBefore time.Time
	// QuietBefore: the owner must not have authenticated since this instant.
	// This is the anti-race gate, not a second staleness gate — see keepalive.go.
	QuietBefore time.Time
	// AttemptBefore: the arm must not have tried this owner since this instant.
	AttemptBefore time.Time
	// FailureBefore: an outstanding failure older than this has cooled down.
	FailureBefore time.Time
	// Limit caps the rows returned, which caps the Tesla OAuth calls one pass
	// can make.
	Limit int
}

// queryStaleTokenOwners selects the owners due for a keepalive rotation.
//
// $1 StaleBefore, $2 QuietBefore, $3 AttemptBefore, $4 FailureBefore, $5 limit.
//
// EVERY PREDICATE IS A REFUSAL TO ACT, and each one is load-bearing:
//
//   - THE SUSPENSION JOIN IS INNER AND DEMANDS suspended_at IS NOT NULL. This
//     arm exists for the one population that generates no Tesla calls of its
//     own. An owner whose car still streams refreshes on demand and needs
//     nothing from us; widening this join would turn a background repair into a
//     background rotation of the whole fleet's credentials.
//
//   - THE OAUTH JOIN IS INNER, so an Apple-native account with no linked Tesla
//     row is unreachable here rather than merely unlikely, and a row with no
//     stored refresh token is excluded outright — there is nothing to spend.
//
//   - expires_at IS NOT NULL. A NULL expiry is not "infinitely stale", it is
//     UNKNOWN, and this arm never acts on unknown. (It is also a row the
//     on-demand path treats as never-expiring and therefore never refreshes,
//     which is a separate pre-existing hazard and not this query's to fix.)
//
//   - THE ACTIVITY JOIN IS LEFT, and its NULL arm ADMITS the owner. That is the
//     opposite of migration 0043's reasoning for the suspend sweeper's inner
//     join, and deliberately so: there the missing row meant "never observed,
//     so do not take an irreversible action", while here the action is a token
//     refresh that costs nothing and undoes nothing. An owner with no activity
//     row whose cars are already suspended is as dormant as this platform can
//     establish, and refusing to keep their grant alive would strand exactly
//     the accounts most likely to be stranded already.
//
//   - THE COOLDOWN HAS TWO RELEASES, time and change. `failed_token_expiry IS
//     DISTINCT FROM a."expires_at"` is the change one: anything that rotated the
//     grant since we gave up on it — a re-link, a reconnect, any on-demand
//     refresh — means the reason for the cooldown is gone. IS DISTINCT FROM
//     rather than <> so a NULL on either side reads as "different" and admits
//     the owner, which is the safe direction for a gate whose job is to stop us
//     retrying a DEAD grant, not to stop us retrying a live one.
//
// ORDERED BY THE CLOCK, STALEST FIRST, so a backlog against the per-pass cap
// spends its attempts on the grants closest to lapsing.
//
// GROUP BY, because an owner with three suspended cars is still one owner and
// one grant. MIN() picks the earliest expiry in the schema-permitted case of two
// Tesla rows for one user; the rotation itself is per-user, not per-row.
//
// #nosec G101 -- column-name SQL, not a credential. gosec greps the constant
// for "refresh_token" and flags the literal as a hardcoded credential, exactly
// as it does for queryTeslaToken and queryUpdateTeslaToken.
const queryStaleTokenOwners = `
SELECT v."userId", MIN(a."expires_at") AS token_expires_at
FROM "Vehicle" v
JOIN go_vehicle_telemetry_suspensions s
  ON s.vehicle_id = v."id" AND s.suspended_at IS NOT NULL
JOIN "Account" a
  ON a."userId" = v."userId" AND a."provider" = 'tesla'
LEFT JOIN go_user_activity ua
  ON ua.user_id = v."userId"
LEFT JOIN go_tesla_token_keepalive k
  ON k.user_id = v."userId"
WHERE a."refresh_token_enc" IS NOT NULL
  AND a."refresh_token_enc" <> ''
  AND a."expires_at" IS NOT NULL
  AND to_timestamp(a."expires_at") <= $1
  AND (ua.last_seen_at IS NULL OR ua.last_seen_at <= $2)
  AND (k.last_attempt_at IS NULL OR k.last_attempt_at <= $3)
  AND (k.last_failure_at IS NULL
       OR k.last_failure_at <= $4
       OR k.failed_token_expiry IS DISTINCT FROM a."expires_at")
GROUP BY v."userId"
ORDER BY token_expires_at ASC
LIMIT $5`

// queryRecordKeepaliveSuccess stamps a rotation that landed and CLEARS the
// outstanding failure. Clearing is the point: an account that has come back
// starts from a clean sheet, so a future refusal gets its own full cooldown
// rather than inheriting a stale one.
const queryRecordKeepaliveSuccess = `
INSERT INTO go_tesla_token_keepalive (user_id, last_attempt_at, last_success_at)
VALUES ($1, $2, $2)
ON CONFLICT (user_id) DO UPDATE
   SET last_attempt_at     = EXCLUDED.last_attempt_at,
       last_success_at     = EXCLUDED.last_success_at,
       last_failure_at     = NULL,
       failed_token_expiry = NULL`

// queryRecordKeepaliveFailure stamps a rotation that did not land, pinning the
// token generation it failed on.
//
// last_success_at is left ALONE — a failure today does not un-happen a success
// last month, and an operator asking "did this ever work?" needs both answers.
const queryRecordKeepaliveFailure = `
INSERT INTO go_tesla_token_keepalive (user_id, last_attempt_at, last_failure_at, failed_token_expiry)
VALUES ($1, $2, $2, $3)
ON CONFLICT (user_id) DO UPDATE
   SET last_attempt_at     = EXCLUDED.last_attempt_at,
       last_failure_at     = EXCLUDED.last_failure_at,
       failed_token_expiry = EXCLUDED.failed_token_expiry`

// TeslaTokenKeepaliveRepo owns go_tesla_token_keepalive and the stale-grant
// candidate read.
type TeslaTokenKeepaliveRepo struct {
	pool *pgxpool.Pool
}

// NewTeslaTokenKeepaliveRepo wires the repo to a pool.
func NewTeslaTokenKeepaliveRepo(pool *pgxpool.Pool) *TeslaTokenKeepaliveRepo {
	return &TeslaTokenKeepaliveRepo{pool: pool}
}

// ListStaleTokenOwners returns up to q.Limit dormant owners whose Tesla grant
// has not been rotated since q.StaleBefore, stalest first.
func (r *TeslaTokenKeepaliveRepo) ListStaleTokenOwners(
	ctx context.Context, q StaleTokenOwnerQuery,
) ([]StaleTokenOwner, error) {
	rows, err := r.pool.Query(ctx, queryStaleTokenOwners,
		q.StaleBefore, q.QuietBefore, q.AttemptBefore, q.FailureBefore, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("store.TeslaTokenKeepaliveRepo.ListStaleTokenOwners: query: %w", err)
	}
	defer rows.Close()

	var out []StaleTokenOwner
	for rows.Next() {
		var (
			ownerID string
			expiry  int64
		)
		if err := rows.Scan(&ownerID, &expiry); err != nil {
			return nil, fmt.Errorf("store.TeslaTokenKeepaliveRepo.ListStaleTokenOwners: scan: %w", err)
		}
		out = append(out, StaleTokenOwner{
			OwnerID:         ownerID,
			TokenExpiresAt:  time.Unix(expiry, 0).UTC(),
			TokenExpiryUnix: expiry,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.TeslaTokenKeepaliveRepo.ListStaleTokenOwners: rows: %w", err)
	}
	return out, nil
}

// RecordKeepaliveSuccess stamps a rotation that landed and clears any
// outstanding failure for the owner.
func (r *TeslaTokenKeepaliveRepo) RecordKeepaliveSuccess(ctx context.Context, userID string, at time.Time) error {
	if _, err := r.pool.Exec(ctx, queryRecordKeepaliveSuccess, userID, at); err != nil {
		return fmt.Errorf("store.TeslaTokenKeepaliveRepo.RecordKeepaliveSuccess(%s): %w", userID, err)
	}
	return nil
}

// RecordKeepaliveFailure stamps a rotation that did not land, pinning the token
// generation (`tokenExpiryUnix`) it failed on so the cooldown can be released
// early if anything rotates the grant in the meantime.
func (r *TeslaTokenKeepaliveRepo) RecordKeepaliveFailure(
	ctx context.Context, userID string, at time.Time, tokenExpiryUnix int64,
) error {
	if _, err := r.pool.Exec(ctx, queryRecordKeepaliveFailure, userID, at, tokenExpiryUnix); err != nil {
		return fmt.Errorf("store.TeslaTokenKeepaliveRepo.RecordKeepaliveFailure(%s): %w", userID, err)
	}
	return nil
}
