package main

// The boundary between telemetry.TeslaTokenRotator (the on-demand refresh's
// consumer-site seam) and store.RotateTeslaTokenLockedWaiting (the transaction
// and row lock that implement it), MYR-595. Same shape as
// fleetSuspendRotator's, for the keepalive's NOWAIT sibling.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// teslaTokenLockWait is how long an on-demand refresh queues for the account's
// row lock before giving up. It has to comfortably outlast a Tesla OAuth
// round-trip, since the whole point of waiting is that the holder of the lock is
// making one and we want its answer instead of a second one. Beyond that it is a
// backstop against a wedged transaction, and the caller's own request deadline
// is the real ceiling.
const teslaTokenLockWait = 5 * time.Second

// teslaTokenRotatorAdapter serializes one user's on-demand token refresh.
type teslaTokenRotatorAdapter struct {
	repo *store.AccountRepo
}

// RotateTeslaToken runs `rotate` inside the store's locked transaction,
// translating the two layers' token shapes and the busy sentinel at the seam so
// internal/telemetry never imports internal/store.
func (a *teslaTokenRotatorAdapter) RotateTeslaToken(
	ctx context.Context,
	userID string,
	rotate func(ctx context.Context, stored telemetry.TeslaToken) (telemetry.TeslaToken, bool, error),
) (telemetry.TeslaToken, error) {
	snap, err := a.repo.RotateTeslaTokenLockedWaiting(ctx, userID, teslaTokenLockWait,
		func(ctx context.Context, stored store.TeslaTokenSnapshot) (store.TeslaTokenPair, bool, error) {
			next, rotated, rErr := rotate(ctx, teslaTokenFromSnapshot(stored))
			if rErr != nil || !rotated {
				return store.TeslaTokenPair{}, false, rErr
			}
			return store.TeslaTokenPair{
				AccessToken:  next.AccessToken,
				RefreshToken: next.RefreshToken,
				ExpiresAt:    next.ExpiresAt.Unix(),
			}, true, nil
		})
	if errors.Is(err, store.ErrTeslaTokenRowBusy) {
		return telemetry.TeslaToken{}, fmt.Errorf("rotate tesla token(user=%s): %w", userID, telemetry.ErrTeslaTokenRowBusy)
	}
	if err != nil {
		return telemetry.TeslaToken{}, err
	}
	return teslaTokenFromSnapshot(snap), nil
}

// teslaTokenFromSnapshot converts the store's epoch expiry to the telemetry
// layer's time.Time, mapping the column's 0/NULL onto the zero time that means
// "expiry unknown" there — the same reading teslaTokenAdapter takes.
func teslaTokenFromSnapshot(snap store.TeslaTokenSnapshot) telemetry.TeslaToken {
	tok := telemetry.TeslaToken{
		AccessToken:  snap.AccessToken,
		RefreshToken: snap.RefreshToken,
	}
	if snap.ExpiresAt != 0 {
		tok.ExpiresAt = time.Unix(snap.ExpiresAt, 0)
	}
	return tok
}
