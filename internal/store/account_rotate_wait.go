package store

// The ON-DEMAND half of the serialized Tesla token rotation (MYR-595).
//
// account_rotate.go serializes the BACKGROUND keepalive and names, in its own
// package doc, the window it leaves open: the on-demand path reads its refresh
// token with a plain SELECT and only meets a lock at the blind UPDATE, which is
// after its Tesla call. Two on-demand callers for one account can therefore both
// read the same single-use refresh token and both spend it. The loser gets
// `invalid_grant` and the user gets told to re-link an account that is fine.
//
// This file closes that window with the same transaction-and-row-lock machinery,
// and differs from the keepalive's entry point in exactly two ways — both of
// them because a user is waiting on the other end:
//
//   - IT WAITS FOR THE LOCK instead of abandoning a contended row. A background
//     arm that queued would rotate a grant the instant a user's own reconnect
//     finished with it; an on-demand caller that walked away would fail a
//     request that was about to be satisfied by whoever holds the lock. The wait
//     is bounded by `lock_timeout` (SET LOCAL, so it bounds the LOCK ACQUISITION
//     ONLY and not the caller's Tesla call), and a wedged transaction therefore
//     surfaces as ErrTeslaTokenRowBusy rather than hanging a request forever.
//
//   - THE CALLBACK IS HANDED THE WHOLE STORED PAIR, not just the refresh token,
//     and may decline to rotate. That is what makes waiting safe: the second
//     caller through the lock re-reads the row INSIDE it, sees the pair the
//     first caller just committed, and returns that instead of immediately
//     spending the brand-new single-use refresh token it is looking at. Without
//     the re-read, serializing two racing refreshes would only convert them into
//     two rotations in a row.
//
// Expiry policy lives with the caller (internal/telemetry), not here: this file
// hands over what is stored and stores what comes back.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TeslaTokenSnapshot is the stored Tesla OAuth pair, decrypted, as read under
// the row lock. ExpiresAt is Unix seconds and is 0 when the column is NULL —
// "unknown", the same thing the plain read path reports for it.
type TeslaTokenSnapshot struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

// queryLockTeslaTokenWaiting reads the whole stored pair and takes an exclusive
// row lock, QUEUEING for it rather than failing fast. The wait is bounded by the
// lock_timeout set on the transaction immediately before this runs; without that
// SET LOCAL this statement would block for as long as the other writer lives.
//
// LIMIT 1 + ORDER BY "id" match queryLockTeslaToken exactly, so both lock
// entry points choose the same row in the schema-permitted (never-observed) case
// of two Tesla rows for one user.
//
// #nosec G101 -- column-name SQL, not a credential.
const queryLockTeslaTokenWaiting = `SELECT "access_token_enc", "refresh_token_enc", "expires_at"
FROM "Account"
WHERE "userId" = $1 AND "provider" = 'tesla'
ORDER BY "id"
LIMIT 1
FOR UPDATE`

// querySetLockTimeout bounds ONLY the lock acquisition, and only for this
// transaction. set_config's third argument is is_local, so it is reverted on
// commit or rollback and the pooled connection is handed back unchanged.
const querySetLockTimeout = `SELECT set_config('lock_timeout', $1, true)`

// defaultTeslaTokenLockWait is the ceiling on how long an on-demand refresh
// queues behind another refresh of the SAME account. Long enough to cover a
// Tesla OAuth round-trip made by whoever holds the lock (which is the case this
// wait exists to win), short enough that a wedged transaction costs a user a
// bounded pause instead of an open-ended one.
const defaultTeslaTokenLockWait = 5 * time.Second

// RotateTeslaTokenLockedWaiting spends the user's stored Tesla refresh token
// under a row lock it QUEUES for, and returns the pair that is on file when it
// is done — which may be the pair it stored, or the one it found.
//
// The callback is handed the stored pair as read INSIDE the lock and returns
// (pair, rotated, err):
//
//   - rotated=true stores `pair` in the same transaction as the read. Nothing
//     else can have committed a write to that row in between.
//   - rotated=false writes NOTHING and the stored pair is returned unchanged.
//     This is the double-check arm: a caller that finds the row already carrying
//     a usable token declines rather than spending the replacement it can see.
//   - err writes nothing and is returned verbatim-wrapped. A failure to STORE a
//     pair Tesla did mint aborts the transaction and surfaces as an error, so no
//     caller is ever handed a token whose durability is a lie.
//
// lockWait bounds the lock acquisition; pass 0 for defaultTeslaTokenLockWait.
// Exceeding it yields ErrTeslaTokenRowBusy with the refresh token never read.
// The caller's own ctx still bounds everything else, including the callback.
//
// Errors: ErrTeslaTokenNotFound when there is no Tesla row, ErrTeslaTokenRowBusy
// when the lock could not be taken within lockWait, and the callback's own error
// otherwise.
func (r *AccountRepo) RotateTeslaTokenLockedWaiting(
	ctx context.Context,
	userID string,
	lockWait time.Duration,
	rotate func(ctx context.Context, stored TeslaTokenSnapshot) (TeslaTokenPair, bool, error),
) (TeslaTokenSnapshot, error) {
	if lockWait <= 0 {
		lockWait = defaultTeslaTokenLockWait
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return TeslaTokenSnapshot{}, fmt.Errorf("AccountRepo.RotateTeslaTokenLockedWaiting(user=%s): begin: %w", userID, err)
	}
	// Also the release path for the declined-rotation case below: nothing was
	// written, so rolling back is both correct and cheaper than a commit.
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	stored, err := r.lockedTeslaSnapshot(ctx, tx, userID, lockWait)
	if err != nil {
		return TeslaTokenSnapshot{}, err
	}

	pair, rotated, err := rotate(ctx, stored)
	if err != nil {
		return TeslaTokenSnapshot{}, fmt.Errorf("AccountRepo.RotateTeslaTokenLockedWaiting(user=%s): rotate: %w", userID, err)
	}
	if !rotated {
		return stored, nil
	}

	if err := writeRotatedPair(ctx, tx, r, userID, pair); err != nil {
		return TeslaTokenSnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		// Tesla has already consumed the old refresh token, so the pair that
		// just failed to land is the only live one. Returning the error rather
		// than the token is the point: an in-memory-only token would work once
		// and leave the stored pair permanently dead with nobody the wiser.
		return TeslaTokenSnapshot{}, fmt.Errorf("AccountRepo.RotateTeslaTokenLockedWaiting(user=%s): commit: %w", userID, err)
	}

	// Identical field-for-field with TeslaTokenPair, and deliberately a distinct
	// type: a pair is what a rotation PRODUCES, a snapshot is what is ON FILE.
	return TeslaTokenSnapshot(pair), nil
}

// lockedTeslaSnapshot sets the lock budget, takes the row lock and returns the
// decrypted stored pair.
func (r *AccountRepo) lockedTeslaSnapshot(
	ctx context.Context, tx pgx.Tx, userID string, lockWait time.Duration,
) (TeslaTokenSnapshot, error) {
	const wrap = "AccountRepo.RotateTeslaTokenLockedWaiting(user=%s)"

	if _, err := tx.Exec(ctx, querySetLockTimeout, fmt.Sprintf("%dms", lockWait.Milliseconds())); err != nil {
		return TeslaTokenSnapshot{}, fmt.Errorf(wrap+": lock budget: %w", userID, err)
	}

	var accessEnc, refreshEnc *string
	var expiresAt *int64
	err := tx.QueryRow(ctx, queryLockTeslaTokenWaiting, userID).Scan(&accessEnc, &refreshEnc, &expiresAt)

	// lock_timeout raises the SAME SQLSTATE (55P03) NOWAIT does, so a wait that
	// ran out of budget and a row that was busy on arrival are one condition
	// with one sentinel — and in both cases nothing was read or spent.
	if isLockNotAvailable(err) {
		return TeslaTokenSnapshot{}, fmt.Errorf(wrap+": %w", userID, ErrTeslaTokenRowBusy)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return TeslaTokenSnapshot{}, fmt.Errorf(wrap+": %w", userID, ErrTeslaTokenNotFound)
	}
	if err != nil {
		return TeslaTokenSnapshot{}, fmt.Errorf(wrap+": lock: %w", userID, err)
	}

	access, err := r.resolveTokenValue(accessEnc)
	if err != nil {
		return TeslaTokenSnapshot{}, fmt.Errorf(wrap+": access column: %w", userID, err)
	}
	refresh, err := r.resolveTokenValue(refreshEnc)
	if err != nil {
		return TeslaTokenSnapshot{}, fmt.Errorf(wrap+": refresh column: %w", userID, err)
	}

	snap := TeslaTokenSnapshot{AccessToken: access, RefreshToken: refresh}
	if expiresAt != nil {
		snap.ExpiresAt = *expiresAt
	}
	return snap, nil
}
