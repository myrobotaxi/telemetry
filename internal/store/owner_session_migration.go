package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// MYR-488 — an identity convergence must carry the person's LIVE SESSION with
// it, or the device it is running on becomes a zombie.
//
// THE INCIDENT. rebindApple re-points go_identity_apple onto the canonical user
// and records the edge, and until now that was the whole of it. The device that
// was signed in went on holding a refresh-token family filed under the ABANDONED
// id, and nothing ever moved it: go_refresh_tokens is keyed on user_id, the
// convergence never touched it, and no code path re-issues the pair. The device
// kept refreshing successfully against a subject that, post-convergence, owns
// nothing at all — no vehicles, no shares, no settings — so every read came back
// confidently empty while the cached profile still rendered. James Guan's
// 13:45:39Z convergence (cd38cb3e… -> cmnccy4tf…) left zero refresh rows under
// the target and a phone that could not be talked out of it.
//
// WHY MIGRATE RATHER THAN REVOKE. The two ids are the same human, and the proof
// is the thing that just happened: the Apple binding MOVED, which rebindApple
// only does on ownership/email evidence and only records after checking that a
// row was actually re-pointed. A convergence edge is already treated by this
// codebase as an unconditional grant of DELETE authority over the target
// (account_deletion_scope.go) — carrying a live session across the same edge is
// strictly weaker than the authority the edge already conveys. Revoking instead
// would be defensible but worse: it signs a person out in the middle of the
// Tesla link they just completed, at the exact moment the app is about to show
// them their car for the first time.
//
// WHY THE STATEMENT LIVES IN store AND NOT identity. go_refresh_tokens belongs
// to internal/identity. But the whole point is that the move and the re-point
// must never be observable apart, and the transaction that owns the re-point is
// OwnerProvisioner's. A second connection, a second transaction, or an
// after-the-fact repair job all reintroduce a window in which the binding has
// moved and the session has not — which IS the bug. So the write is made from
// here, inside that transaction, and identity keeps ownership of every other
// statement against the table.

// maxRefreshMigrationPasses caps the sweep. The sweep repeats until a pass finds
// nothing left under the abandoned id, and THAT ZERO-ROW PASS IS THE POINT: it
// is the proof, taken at a fresh READ COMMITTED snapshot, that no concurrent
// rotation re-planted a row behind the previous one. Without it the code would
// be assuming what it should be checking.
//
// The assumption is nearly always right. FOR UPDATE row locks are held to end of
// transaction, so a rotation either fully commits before our locking SELECT
// returns — in which case the first UPDATE's later snapshot already sees
// everything it inserted — or it blocks until we commit and then re-reads a
// user_id that has moved. The gap is narrow: a rotation of a row our SELECT
// never saw (because an earlier rotation inserted it mid-scan) and therefore
// never locked, landing between the SELECT and the UPDATE.
//
// The cap exists because a device re-planting rows faster than we sweep must not
// spin a Tesla link's transaction indefinitely, and whatever the cap fails to
// prove settled is revoked rather than abandoned — see revokeSplitRefreshFamilies.
const maxRefreshMigrationPasses = 3

// reasonConvergedSplit is the revoke reason for the pathological tail: a family
// that a concurrent rotation kept re-planting under the abandoned id faster than
// the sweep could move it. It is the same opaque P0 enum the reason column
// already carries ('rotated' | 'revoked' | 'reuse_detected' | 'account_deleted').
const reasonConvergedSplit = "converged_split"

// queryLockRefreshRowsForUser reads — and LOCKS — every refresh row filed under
// the abandoned id, projecting the per-row "this family is still alive" flag.
//
// FOR UPDATE is the ordering primitive, not a formality. It is what forces a
// rotation that has not yet started to wait for the convergence to commit and
// then re-read a user_id that has changed underneath it (READ COMMITTED
// re-evaluates a locked row once the lock is released), and it is what makes the
// reverse interleaving BLOCK here rather than interleave silently.
//
// The predicate is evaluated in SQL, against the database clock, because the
// alternative — comparing expires_at to the server's time.Now() — decides
// whether a person stays signed in using two clocks that are not the same one.
//
// IT DELIBERATELY DOES NOT TEST rotated_to. "Alive" here is NOT "this row can be
// rotated" — it is "this family still has something worth carrying across". The
// distinction is load-bearing and cost a failing race before it was understood:
// when a rotation grabs the tip's lock first, this SELECT blocks, and by the
// time it is released the tip it can see has been marked SPENT while the
// successor that replaced it was inserted after the statement's snapshot and is
// invisible. A `rotated_to IS NULL` term reads that as a dead family and
// migrates nothing at all — leaving behind exactly the orphaned live session
// this whole file exists to prevent.
//
// Read the other way it is also simply the more honest test: rotation always
// inserts the successor in the same transaction that marks its predecessor
// spent, so an unrevoked, unexpired row IS proof of a live family whether or not
// this statement can see the tip. Conversely a family is dead only when every
// row is revoked (revokeFamilyTx marks them all) or every row is expired (each
// rotation extends the expiry, so the newest row is always the last to go).
//
// It is a per-row expression rather than a bool_or aggregate because FOR UPDATE
// cannot be combined with GROUP BY; the OR is done in Go, over a handful of rows.
//
// #nosec G101 -- column/predicate SQL over a hash-only table, not a credential
// (gosec greps the literal 'refresh_tokens' and misflags it).
const queryLockRefreshRowsForUser = `
SELECT family_id,
       (revoked = FALSE AND expires_at > NOW()) AS alive
FROM go_refresh_tokens
WHERE user_id = $1
FOR UPDATE`

// queryMigrateRefreshFamilies re-files whole families onto the canonical id.
//
// KEYED ON family_id, NOT ON THE ROW PREDICATE. Rotation lineage is the unit of
// meaning here: reuse-detection revokes by family_id, and a family split across
// two user_ids would revoke correctly but attribute the theft to whichever half
// the presented token happened to sit in. A family moves whole or not at all.
//
// The `user_id = $2` predicate is the anti-double-migrate guard: a re-run, or a
// second pass, moves zero rows because the rows are no longer under the
// abandoned id. It also means the statement can never drag a row belonging to a
// third party along with the family.
//
// #nosec G101 -- column/predicate SQL over a hash-only table, not a credential.
const queryMigrateRefreshFamilies = `
UPDATE go_refresh_tokens
SET user_id = $1
WHERE user_id = $2 AND family_id = ANY($3)`

// queryRevokeSplitRefreshFamilies is the terminator. Anything the capped sweep
// could not move is revoked rather than left behind, because "left behind" is
// precisely the defect: an orphaned live family is a device that authenticates
// forever as an id that owns nothing. A revoked one is a device that gets an
// honest failure on its next refresh and takes the person to sign-in.
//
// #nosec G101 -- column/predicate SQL over a hash-only table, not a credential.
const queryRevokeSplitRefreshFamilies = `
UPDATE go_refresh_tokens
SET revoked = TRUE, revoked_at = NOW(), reason = $3
WHERE user_id = $1 AND family_id = ANY($2) AND revoked = FALSE`

// migrateRefreshFamilies moves the abandoned id's LIVE refresh-token families
// onto the canonical id, inside the caller's transaction.
//
// WHAT MOVES, AND WHAT DELIBERATELY DOES NOT:
//
//   - A family that still holds an unrevoked, unexpired row moves in full —
//     spent ancestors included, so family_id continuity holds and the device's
//     very next rotation works without noticing anything happened.
//   - A REVOKED family stays where it is and stays revoked. Revocation is a
//     decision somebody made — a sign-out, or reuse-detection catching a stolen
//     token — and a convergence is not a reason to revisit it. Moving it would
//     also relocate the evidence of a theft away from the identity it happened
//     under. Nothing here ever clears revoked, so no convergence can resurrect a
//     dead family.
//   - An EXPIRED family stays where it is. There is no session left to preserve;
//     the person has to sign in again either way, and that sign-in will resolve
//     through the moved Apple binding to the canonical id.
func (p *OwnerProvisioner) migrateRefreshFamilies(ctx context.Context, tx pgx.Tx, fromUserID, toUserID string) error {
	families, err := lockLiveRefreshFamilies(ctx, tx, fromUserID)
	if err != nil {
		return fmt.Errorf("store.ProvisionTeslaOwner: migrate sessions %s->%s: %w", fromUserID, toUserID, err)
	}
	if len(families) == 0 {
		return nil
	}

	var moved int64
	settled := false
	for pass := 0; pass < maxRefreshMigrationPasses; pass++ {
		tag, execErr := tx.Exec(ctx, queryMigrateRefreshFamilies, toUserID, fromUserID, families)
		if execErr != nil {
			return fmt.Errorf("store.ProvisionTeslaOwner: migrate sessions %s->%s: %w", fromUserID, toUserID, execErr)
		}
		if tag.RowsAffected() == 0 {
			settled = true
			break
		}
		moved += tag.RowsAffected()
	}

	stranded, err := p.revokeSplitRefreshFamilies(ctx, tx, fromUserID, families, settled)
	if err != nil {
		return err
	}

	// Audit (P0 only: opaque cuids and counts; no hashes, no tokens).
	p.logger.Info("owner_sessions_migrated",
		slog.String("event", "owner_sessions_migrated"),
		slog.String("from_user_id", fromUserID),
		slog.String("to_user_id", toUserID),
		slog.Int("families", len(families)),
		slog.Int64("rows_moved", moved),
		slog.Int64("rows_revoked_split", stranded),
	)
	return nil
}

// revokeSplitRefreshFamilies revokes whatever the sweep left under the abandoned
// id. It is skipped entirely when the sweep settled — the overwhelmingly common
// case — so the ordinary convergence pays for two statements, not three.
func (p *OwnerProvisioner) revokeSplitRefreshFamilies(
	ctx context.Context, tx pgx.Tx, fromUserID string, families []string, settled bool,
) (int64, error) {
	if settled {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, queryRevokeSplitRefreshFamilies, fromUserID, families, reasonConvergedSplit)
	if err != nil {
		return 0, fmt.Errorf("store.ProvisionTeslaOwner: revoke split sessions for %s: %w", fromUserID, err)
	}
	if tag.RowsAffected() > 0 {
		p.logger.Warn("owner_session_migration_split",
			slog.String("event", "owner_session_migration_split"),
			slog.String("from_user_id", fromUserID),
			slog.Int64("rows_revoked", tag.RowsAffected()),
			slog.String("reason", "concurrent rotation outran the migration sweep"),
		)
	}
	return tag.RowsAffected(), nil
}

// lockLiveRefreshFamilies locks the abandoned id's refresh rows and returns the
// family ids that are still alive, in first-seen order so the statement
// parameters, the logs and the tests are all deterministic.
func lockLiveRefreshFamilies(ctx context.Context, tx pgx.Tx, userID string) ([]string, error) {
	rows, err := tx.Query(ctx, queryLockRefreshRowsForUser, userID)
	if err != nil {
		return nil, fmt.Errorf("lock refresh families: %w", err)
	}
	defer rows.Close()

	var order []string
	live := make(map[string]bool)
	for rows.Next() {
		var familyID string
		var alive bool
		if scanErr := rows.Scan(&familyID, &alive); scanErr != nil {
			return nil, fmt.Errorf("scan refresh family: %w", scanErr)
		}
		if _, seen := live[familyID]; !seen {
			order = append(order, familyID)
			live[familyID] = false
		}
		if alive {
			live[familyID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate refresh families: %w", err)
	}

	out := make([]string, 0, len(order))
	for _, familyID := range order {
		if live[familyID] {
			out = append(out, familyID)
		}
	}
	return out, nil
}
