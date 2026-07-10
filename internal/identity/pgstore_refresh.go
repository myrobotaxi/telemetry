package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const queryInsertRefreshRoot = `
INSERT INTO go_refresh_tokens (token_hash, family_id, user_id, expires_at, rotated_from)
VALUES ($1, $2, $3, $4, NULL)`

// InsertRefreshRoot persists the first refresh token of a new family (issued
// at sign-in). newID() supplies the family_id at the call site.
func (s *PgStore) InsertRefreshRoot(ctx context.Context, tokenHash, familyID, userID string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx, queryInsertRefreshRoot, tokenHash, familyID, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("identity.InsertRefreshRoot: %w", err)
	}
	return nil
}

// RotateRefreshToken atomically consumes oldHash and issues newHash in the
// same family. It locks the old row (SELECT ... FOR UPDATE) so two concurrent
// refreshes cannot both succeed: the first rotates; the second observes a
// spent row and is treated as reuse (the family is revoked). Outcomes:
//
//   - RotateInvalid : oldHash unknown.
//   - RotateExpired : oldHash known, unspent, but past expiry (no revoke).
//   - RotateReuse   : oldHash already spent or revoked -> family revoked.
//   - RotateRotated : success; new row inserted, old row marked spent.
func (s *PgStore) RotateRefreshToken(ctx context.Context, oldHash, newHash string, newExpiresAt time.Time) (RotateResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RotateResult{}, fmt.Errorf("identity.RotateRefreshToken: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	var (
		familyID  string
		userID    string
		expiresAt time.Time
		rotatedTo *string
		revoked   bool
	)
	const selectForUpdate = `
SELECT family_id, user_id, expires_at, rotated_to, revoked
FROM go_refresh_tokens WHERE token_hash = $1 FOR UPDATE`
	err = tx.QueryRow(ctx, selectForUpdate, oldHash).Scan(&familyID, &userID, &expiresAt, &rotatedTo, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return RotateResult{Outcome: RotateInvalid}, nil
	}
	if err != nil {
		return RotateResult{}, fmt.Errorf("identity.RotateRefreshToken: select: %w", err)
	}

	// Spent (already rotated) or explicitly revoked => reuse/theft.
	if revoked || (rotatedTo != nil && *rotatedTo != "") {
		if err := revokeFamilyTx(ctx, tx, familyID, "reuse_detected"); err != nil {
			return RotateResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RotateResult{}, fmt.Errorf("identity.RotateRefreshToken: commit reuse: %w", err)
		}
		return RotateResult{Outcome: RotateReuse, UserID: userID, FamilyID: familyID}, nil
	}

	// Natural expiry — benign, not an attack. Leave the family intact.
	if !expiresAt.After(time.Now().UTC()) {
		return RotateResult{Outcome: RotateExpired, UserID: userID, FamilyID: familyID}, nil
	}

	const markSpent = `UPDATE go_refresh_tokens SET rotated_to = $2, reason = 'rotated' WHERE token_hash = $1`
	if _, err := tx.Exec(ctx, markSpent, oldHash, newHash); err != nil {
		return RotateResult{}, fmt.Errorf("identity.RotateRefreshToken: mark spent: %w", err)
	}
	const insertNext = `
INSERT INTO go_refresh_tokens (token_hash, family_id, user_id, expires_at, rotated_from)
VALUES ($1, $2, $3, $4, $5)`
	if _, err := tx.Exec(ctx, insertNext, newHash, familyID, userID, newExpiresAt, oldHash); err != nil {
		return RotateResult{}, fmt.Errorf("identity.RotateRefreshToken: insert next: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RotateResult{}, fmt.Errorf("identity.RotateRefreshToken: commit: %w", err)
	}
	return RotateResult{Outcome: RotateRotated, UserID: userID, FamilyID: familyID}, nil
}

// RevokeFamilyByToken revokes the whole family the given token belongs to
// (explicit sign-out via /api/auth/revoke). Returns found=false when the
// token is unknown so the handler can respond without leaking existence.
func (s *PgStore) RevokeFamilyByToken(ctx context.Context, tokenHash string) (userID string, found bool, err error) {
	const selectFamily = `SELECT family_id, user_id FROM go_refresh_tokens WHERE token_hash = $1`
	var familyID string
	err = s.pool.QueryRow(ctx, selectFamily, tokenHash).Scan(&familyID, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("identity.RevokeFamilyByToken: select: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("identity.RevokeFamilyByToken: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := revokeFamilyTx(ctx, tx, familyID, "revoked"); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("identity.RevokeFamilyByToken: commit: %w", err)
	}
	return userID, true, nil
}

// revokeFamilyTx marks every not-yet-revoked token in a family revoked.
func revokeFamilyTx(ctx context.Context, tx pgx.Tx, familyID, reason string) error {
	const q = `
UPDATE go_refresh_tokens
SET revoked = TRUE, revoked_at = NOW(), reason = $2
WHERE family_id = $1 AND revoked = FALSE`
	if _, err := tx.Exec(ctx, q, familyID, reason); err != nil {
		return fmt.Errorf("identity.revokeFamily(%s): %w", reason, err)
	}
	return nil
}
