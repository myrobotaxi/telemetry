// User-entered display-name write path (MYR-581).
//
// WHY THIS EXISTS AT ALL. Apple supplies a person's name ONLY on the first
// authorization, so accounts routinely live forever with no name — and the
// display fallback then renders them as the make on the incoming ride card
// (MYR-532 item 4: "James requesting ride why does it say Tesla"). MYR-366
// deliberately shipped NO rename affordance; the client REVERSED that on
// 2026-08-17, and this writer is the reversal's storage half.
//
// ── THE BUG THIS FILE'S SECOND VERSION FIXES ─────────────────────────────────
//
// The first version wrote ONE table: `UPDATE "User" SET "name"`. That is the
// wrong table for the exact population the feature exists for. An APPLE-NATIVE
// account is minted in `go_users` + `go_identity_apple`
// (internal/identity/pgstore.go) and has NO Prisma "User" ROW AT ALL, so the
// self-scoped UPDATE matched zero rows and the endpoint answered 404 — to every
// nameless Apple user who tried to set a name. The one endpoint that could clear
// the "Tesla wants a ride" report was unreachable by everyone in the report.
//
// ── WHY IT WRITES EVERY RUNG, AND WHY THAT IS THE WHOLE DESIGN ───────────────
//
// The platform reads a display name through a THREE-RUNG LADDER, in strict
// precedence: Prisma `"User".name` → the Apple first-consent name in
// `go_identity_apple.name` → `go_users.name`. Four readers share it —
// `requesterIdentitySelect` (MYR-229/264), `queryOwnerFirstNameSources`
// (MYR-368), `ownerNameLadderExpr` and `acceptedByNameExpr` (both MYR-581).
//
// A rename therefore cannot just pick "the account's table", because
// PRECEDENCE, NOT EXISTENCE, DECIDES WHAT WINS. Writing only `go_users` — the
// account's own table for an Apple-native user — would leave the Apple
// first-consent name sitting one rung ABOVE it, and every reader would go on
// serving the OLD name while the endpoint reported success. That is the failure
// mode a "write where the account lives" design walks straight into, and it is
// silent.
//
// So this writer updates the name on EVERY RUNG THE ACCOUNT HAS A ROW ON, in one
// transaction. The invariant is then trivial and total: whichever rung the
// COALESCE selects, it carries the new name. There is no precedence to reason
// about, no ordering to get right, and nothing to update when a fifth reader is
// added.
//
// ── WHY NOT A NEW GO-OWNED COLUMN OR TABLE READ FIRST ───────────────────────
//
// The alternative considered was a dedicated go-side display-name store, read as
// a new rung 0 by all four ladders. It was rejected as the LARGER change, not the
// smaller one: it needs a migration, a fourth rung threaded into four SQL
// constants in three files, an entry in the §7.6 account-deletion sequence (a
// name row outliving its owner is a P1 leak the deletion sweep knows nothing
// about), a `data-lifecycle.md` retention entry, and its own down-migration.
// This version needs none of those — no migration, no schema change, no new
// deletion step, and no reader change anywhere, because the rungs it writes are
// the rungs the readers already read.
//
// NO ROW IS EVER CREATED, deliberately. All three statements are UPDATEs. An
// INSERT into `go_users` for an id that lives in `"User"` would fork one human
// across two identity sources — the exact shape MYR-452's convergence table
// exists to clean up after — and would collide with `idx_go_users_email`.
//
// ── ON OVERWRITING THE APPLE FIRST-CONSENT NAME ─────────────────────────────
//
// `go_identity_apple.name` is a DISPLAY CACHE, not a record of Apple's
// assertion: it is populated from a CLIENT-SUPPLIED field on first authorization
// (identity.resolveUser reads `in.FullName`, never a token claim), and
// `TouchAppleLogin` only ever BACKFILLS it (`COALESCE(name, NULLIF($3,”))`).
// That last detail is what makes this write STICK: once set, no sign-in can
// clobber the user's own choice with a stale value.
//
// ── CG-DL-9 ─────────────────────────────────────────────────────────────────
//
// The `"User"` arm remains a §1.4 sanctioned Go-side write carve-out
// (docs/contracts/data-lifecycle.md): one column, one WHERE on the caller's own
// identity, UPDATE only, NO MIGRATION — the same shape as
// `VehicleRepo.UpdateVehicleColor`. CG-DL-9 constrains Go *migration SQL*, and
// this ships none. The other two arms are Go-owned tables and need no carve-out;
// `account_deletion_queries.go` already writes both.
//
// THE NAME IS P1. This file never logs it — no statement here has a log line,
// and the handler logs the EVENT and the user id only.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// queryUpdateUserName writes the caller's OWN display name on the TOP rung, the
// Prisma-owned "User" row. Self-scope is the WHERE clause, not a precondition: an
// unknown id updates zero rows. "updatedAt" is bumped so the Prisma side observes
// the row as changed.
const queryUpdateUserName = `
UPDATE "User"
SET "name"      = $1,
    "updatedAt" = NOW()
WHERE "id" = $2`

// queryUpdateAppleIdentityName writes the MIDDLE rung.
//
// EVERY BINDING ROW FOR THIS USER, not one: `go_identity_apple.user_id` is
// indexed but NOT unique, and the ladder's middle rung picks the most recent
// login (`ORDER BY last_login_at DESC LIMIT 1`). Updating only one row would
// leave the ladder free to select a sibling still carrying the old name, and
// which one it picks would depend on login order — a rename that works or does
// not depending on which device you last signed in on.
const queryUpdateAppleIdentityName = `
UPDATE go_identity_apple
SET name = $1
WHERE user_id = $2`

// queryUpdateGoUserName writes the BOTTOM rung. UPDATE, never upsert: see the
// file header on why no row is ever created here.
const queryUpdateGoUserName = `
UPDATE go_users
SET name = $1, updated_at = NOW()
WHERE id = $2`

// ProfileNameRepo owns the one profile write the platform permits.
type ProfileNameRepo struct {
	pool *pgxpool.Pool
}

func NewProfileNameRepo(pool *pgxpool.Pool) *ProfileNameRepo {
	return &ProfileNameRepo{pool: pool}
}

// UpdateUserName sets the caller's display name on every identity rung the
// account actually has a row on.
//
// The HANDLER owns validation (trim, non-empty, length cap) — this writer only
// refuses the empty string as a second net, LOUDLY: an empty name is not a
// rename, it is a deletion this endpoint does not offer, so a caller that skipped
// validation gets an error rather than a false "not found".
//
// Returns updated=false (nil error) when NO rung matched — which now means
// something much narrower than it used to: the authenticated id has no row in
// `"User"`, `go_identity_apple` OR `go_users`. That is a genuinely orphaned
// token (a deleted account whose access token has not yet expired, or a
// bootstrap-override id that was never provisioned), not the ordinary
// Apple-native account the previous version turned away.
//
// ONE TRANSACTION, and it is load-bearing rather than tidiness. A partial commit
// could leave the NEW name on a lower rung and the OLD name on a higher one,
// which is the precedence bug in the file header arriving by a different route:
// the readers would serve the stale name while the endpoint had reported success.
// All three rungs move together or none does.
func (r *ProfileNameRepo) UpdateUserName(ctx context.Context, userID, name string) (bool, error) {
	if name == "" {
		return false, errors.New("ProfileNameRepo.UpdateUserName: empty name is not a rename")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("ProfileNameRepo.UpdateUserName(user=%s): begin: %w", userID, err)
	}
	// Rollback on every path that has not committed; a no-op after Commit.
	defer func() { _ = tx.Rollback(ctx) }()

	var affected int64
	for _, q := range []string{
		queryUpdateUserName,
		queryUpdateAppleIdentityName,
		queryUpdateGoUserName,
	} {
		tag, execErr := tx.Exec(ctx, q, name, userID)
		if execErr != nil {
			return false, fmt.Errorf("ProfileNameRepo.UpdateUserName(user=%s): %w", userID, execErr)
		}
		affected += tag.RowsAffected()
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("ProfileNameRepo.UpdateUserName(user=%s): commit: %w", userID, err)
	}
	return affected > 0, nil
}
