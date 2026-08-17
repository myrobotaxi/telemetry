// User-entered display-name write path (MYR-581).
//
// This is a NARROW, self-scoped UPDATE against the Prisma-owned "User" table —
// the next sanctioned Go-side write carve-out in the §1.4 family
// (docs/contracts/data-lifecycle.md), after the Vehicle-table quartet
// (provision, teardown, plate, colour) and the account-deletion teardown. The
// same shape as `VehicleRepo.UpdateVehicleColor`: one column, one WHERE on the
// caller's own identity, no migration (the column has existed since v1 — Apple
// populated it on first authorization, or never).
//
// WHY THIS EXISTS AT ALL. Apple supplies a person's name ONLY on the first
// authorization, so accounts routinely live forever with `name` NULL — and the
// display fallback then renders them as "Tesla" on the incoming ride card
// (MYR-532 item 4: "James requesting ride why does it say Tesla"). MYR-366
// deliberately shipped NO rename affordance; the client REVERSED that on
// 2026-08-17 ("editable profile name"), and this writer is the reversal's
// storage half.
//
// Why CG-DL-9 does not fire: it constrains Go *migration SQL*; this ships no
// migration. Application-runtime Prisma UPDATEs are the sanctioned class.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// queryUpdateUserName writes the caller's OWN display name. Self-scope is the
// WHERE clause, not a precondition: an unknown id updates zero rows.
// "updatedAt" is bumped so the Prisma side observes the row as changed.
const queryUpdateUserName = `
UPDATE "User"
SET "name"      = $1,
    "updatedAt" = NOW()
WHERE "id" = $2`

// ProfileNameRepo owns the one profile write the platform permits.
type ProfileNameRepo struct {
	pool *pgxpool.Pool
}

func NewProfileNameRepo(pool *pgxpool.Pool) *ProfileNameRepo {
	return &ProfileNameRepo{pool: pool}
}

// UpdateUserName sets the caller's display name.
//
// The HANDLER owns validation (trim, non-empty, length cap) — this writer only
// refuses the empty string as a second net, LOUDLY: an empty name is not a
// rename, it is a deletion this endpoint does not offer, so a caller that
// skipped validation gets an error rather than a false "not found". Returns
// updated=false (nil error) when no row matched.
func (r *ProfileNameRepo) UpdateUserName(ctx context.Context, userID, name string) (bool, error) {
	if name == "" {
		return false, errors.New("ProfileNameRepo.UpdateUserName: empty name is not a rename")
	}
	tag, err := r.pool.Exec(ctx, queryUpdateUserName, name, userID)
	if err != nil {
		return false, fmt.Errorf("ProfileNameRepo.UpdateUserName(user=%s): %w", userID, err)
	}
	return tag.RowsAffected() > 0, nil
}
