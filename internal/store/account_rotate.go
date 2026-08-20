// Package store — the SERIALIZED Tesla token rotation (MYR-594).
//
// AccountRepo.GetTeslaToken + AccountRepo.UpdateTeslaToken are the on-demand
// pair: read the stored token, and if it has expired, hand the refresh token to
// Tesla and write back whatever comes out. Read the two of them together and
// the hazard is plain — THERE IS NO GUARD BETWEEN THEM. Nothing locks the row,
// nothing compares-and-swaps, nothing serializes two callers. That was tolerable
// while every refresh was driven by a user's own in-flight request, because a
// person is one person and their requests queue behind each other in practice.
//
// MYR-594 adds a SECOND, UNATTENDED driver of the same rotation, and Tesla's
// refresh tokens are SINGLE-USE: presenting one both consumes it and mints its
// replacement. Two callers holding the same refresh token means one of them
// gets a new pair and the other gets `invalid_grant` — and if the loser were to
// write anything, or the winner were to fail to, the account is stranded with a
// dead refresh token and no way back but a full OAuth re-pair. That is worse
// than the lapse MYR-594 exists to prevent, so the keepalive arm does not reuse
// the unguarded pair. It comes through here.
//
// ── WHAT THIS FUNCTION GUARANTEES ───────────────────────────────────────────
//
//   - THE READ AND THE WRITE ARE ONE TRANSACTION, and the read takes a row
//     lock. Between deciding which refresh token to spend and storing its
//     replacement, no other writer can commit a change to that row — including
//     the on-demand path, which takes no lock of its own but whose blind UPDATE
//     is blocked by ours all the same. Postgres row locks are mandatory for
//     writers; a writer cannot opt out by not asking.
//
//   - NOWAIT, SO A CONTENDED ROW IS ABANDONED RATHER THAN QUEUED. If anybody
//     already holds the lock, this returns ErrTeslaTokenRowBusy immediately and
//     THE REFRESH TOKEN IS NEVER EVEN READ, let alone spent. A background arm
//     that waited its turn would be a background arm that rotates a token the
//     instant a user's own reconnect finished with it. Abandoning costs one
//     skipped keepalive against a 45-day margin.
//
//   - NOTHING IS WRITTEN WHEN THE ROTATION FAILS. The callback's error aborts
//     the transaction with the stored pair untouched. Combined with the same
//     property on the on-demand path (both handlers write only on success), it
//     means the database always ends up holding the pair produced by whichever
//     call TESLA actually honoured — the loser of a race writes nothing, so it
//     cannot clobber the winner.
//
// ── THE WINDOW THIS DOES NOT CLOSE, STATED HONESTLY ─────────────────────────
//
// The on-demand path reads its refresh token with a plain SELECT and takes its
// lock only at the UPDATE, which is AFTER its Tesla call. So a caller that has
// already read the token and is mid-flight at Tesla holds no lock, and this
// function will happily acquire one and spend the same token. The outcome is
// still not a stranded account — one of the two calls fails at Tesla and writes
// nothing — but it IS a user-visible error on the on-demand side.
//
// The keepalive arm closes that window by population rather than by locking: it
// only ever looks at owners whose vehicles are suspended AND who have not
// authenticated in days, so the concurrent on-demand caller would have to be a
// request from an account that by construction is making none. See
// internal/fleetsuspend/keepalive.go.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrTeslaTokenRowBusy — another writer holds the account's OAuth row. The
// rotation was NOT attempted and no token was read; the caller should come back
// on a later pass rather than treat this as a failure.
var ErrTeslaTokenRowBusy = errors.New("tesla token row is locked by another writer")

// pgLockNotAvailable is Postgres SQLSTATE 55P03, what FOR UPDATE NOWAIT raises
// when the row is already locked.
const pgLockNotAvailable = "55P03"

// TeslaTokenPair is one rotated Tesla OAuth pair, as returned by the rotation
// callback. ExpiresAt is Unix seconds, matching the column.
type TeslaTokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

// queryLockTeslaToken reads the refresh token ciphertext and takes an exclusive
// row lock in one statement.
//
// LIMIT 1 matches queryTeslaToken's shape, and ORDER BY "id" makes the choice
// deterministic in the (schema-permitted, never-observed) case of two Tesla
// rows for one user — the unique constraint is on (provider, providerAccountId),
// not on (userId, provider). See the caveat on RotateTeslaTokenLocked.
//
// #nosec G101 -- column-name SQL, not a credential. gosec greps the constant
// for "refresh_token" and flags the literal as a hardcoded credential.
const queryLockTeslaToken = `SELECT "refresh_token_enc"
FROM "Account"
WHERE "userId" = $1 AND "provider" = 'tesla'
ORDER BY "id"
LIMIT 1
FOR UPDATE NOWAIT`

// RotateTeslaTokenLocked spends the user's stored Tesla refresh token exactly
// once, under a row lock, and stores the pair that comes back.
//
// The caller supplies the rotation itself as `rotate`, which is handed the
// DECRYPTED refresh token and must return the new pair or an error. That seam
// is what keeps the Tesla HTTP client out of internal/store while leaving the
// lock, the decrypt, the encrypt and the write — every part that touches the
// database — in one place with the on-demand path's own SQL.
//
// THE CALLBACK RUNS INSIDE THE TRANSACTION, holding the row lock, and that is
// deliberate rather than an oversight about long transactions: the whole point
// is that no other writer may commit between reading the token and storing its
// replacement. Bound it with the context. One row of one rarely-touched table
// is locked for the duration, and only for accounts whose cars are already
// suspended.
//
// Errors: ErrTeslaTokenNotFound when there is no Tesla row or no stored refresh
// token, ErrTeslaTokenRowBusy when the row is already locked (nothing was read
// or spent), and the callback's own error verbatim-wrapped otherwise.
//
// CAVEAT, PRE-EXISTING: the write updates every Tesla row for the user while the
// read locks one. The schema permits two only in a state nothing in the product
// creates, and queryTeslaToken has always read with LIMIT 1; this function does
// not make that better or worse.
func (r *AccountRepo) RotateTeslaTokenLocked(
	ctx context.Context,
	userID string,
	rotate func(ctx context.Context, refreshToken string) (TeslaTokenPair, error),
) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): begin: %w", userID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	refresh, err := lockedRefreshToken(ctx, tx, r, userID)
	if err != nil {
		return err
	}

	pair, err := rotate(ctx, refresh)
	if err != nil {
		// NOTHING IS WRITTEN. The stored pair stays exactly as it was, which is
		// the whole failure contract: a refusal from Tesla means the grant is
		// gone, and clobbering the row would destroy the only evidence of which
		// account needs re-pairing without gaining anything.
		return fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): rotate: %w", userID, err)
	}

	if err := writeRotatedPair(ctx, tx, r, userID, pair); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		// Tesla has already consumed the old refresh token by this point, so
		// the new pair is the only live one and it just failed to land. The
		// caller logs this loudly; there is no recovery from here that does not
		// involve the user re-linking.
		return fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): commit: %w", userID, err)
	}
	return nil
}

// lockedRefreshToken takes the row lock and returns the decrypted refresh token.
func lockedRefreshToken(ctx context.Context, tx pgx.Tx, r *AccountRepo, userID string) (string, error) {
	var refreshEnc *string
	err := tx.QueryRow(ctx, queryLockTeslaToken, userID).Scan(&refreshEnc)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgLockNotAvailable {
		return "", fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): %w", userID, ErrTeslaTokenRowBusy)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): lock: %w", userID, err)
	}

	refresh, err := r.resolveTokenValue(refreshEnc)
	if err != nil {
		return "", fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): refresh column: %w", userID, err)
	}
	if refresh == "" {
		// No refresh token on file. There is nothing to spend and nothing to
		// keep alive; the account needs a link, not a rotation.
		return "", fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}
	return refresh, nil
}

// writeRotatedPair encrypts and stores the new pair through the SAME statement
// the on-demand refresh uses — queryUpdateTeslaToken — so the two paths cannot
// drift on which columns a rotation touches or on the plaintext scrub it
// performs while it is there.
func writeRotatedPair(ctx context.Context, tx pgx.Tx, r *AccountRepo, userID string, pair TeslaTokenPair) error {
	accessEnc, err := r.encryptor.EncryptString(pair.AccessToken)
	if err != nil {
		return fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): encrypt access column: %w", userID, err)
	}
	refreshEnc, err := r.encryptor.EncryptString(pair.RefreshToken)
	if err != nil {
		return fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): encrypt refresh column: %w", userID, err)
	}

	tag, err := tx.Exec(ctx, queryUpdateTeslaToken, accessEnc, refreshEnc, pair.ExpiresAt, userID)
	if err != nil {
		return fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): store: %w", userID, err)
	}
	if tag.RowsAffected() == 0 {
		// The row we locked cannot vanish inside our own transaction, so this
		// is unreachable in practice; treat it as the same not-found the
		// on-demand write does rather than committing an empty rotation.
		return fmt.Errorf("AccountRepo.RotateTeslaTokenLocked(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}
	return nil
}
