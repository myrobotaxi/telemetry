package telemetry

// The ON-DEMAND Tesla token path, in one place (MYR-595).
//
// Three surfaces used to carry their own copy of it — the off-request
// TeslaTokenResolver, the fleet-config handler and the vehicle-command handler —
// and all three copies had the same shape and the same hazard: plain SELECT of
// the stored pair, and if the access token has expired, hand the refresh token
// to Tesla and blind-UPDATE whatever comes back. Tesla's refresh tokens are
// SINGLE-USE and rotating, so two of those running at once for one account both
// spend the same token, one of them gets `invalid_grant`, and a user who has
// done nothing wrong is told to re-link their Tesla account.
//
// ── WHAT THIS DOES INSTEAD ──────────────────────────────────────────────────
//
//   - THE FRESH-TOKEN FAST PATH IS UNCHANGED: a plain read, no lock, no
//     transaction. That is what every normal request does, and serializing it
//     would put a row lock in the way of traffic that is not rotating anything.
//
//   - THE REFRESH LEG RUNS INSIDE THE ROW LOCK, through TeslaTokenRotator
//     (store.RotateTeslaTokenLockedWaiting under the adapter). The read of the
//     token being spent, the Tesla call, and the write of its replacement are
//     one transaction; no second refresh can interleave with it.
//
//   - THE LOCK HOLDER RE-CHECKS EXPIRY BEFORE SPENDING ANYTHING. Serializing two
//     racing refreshes is not enough on its own — it would just convert them
//     into two rotations in a row, the second one spending the fresh token the
//     first just minted. So the second arrival looks at what is actually stored
//     now, finds the winner's pair, and RETURNS IT WITHOUT CALLING TESLA.
//
//   - A FAILED PERSIST IS A FAILED REFRESH. The old code logged a store failure
//     and returned the rotated token anyway — a token that works once while the
//     stored pair is already dead, with nobody the wiser until the next request
//     fails for a reason that no longer makes sense. Under the locked rotation
//     the write and the read are the same transaction, so a persist failure
//     aborts it and surfaces as an error, and nothing ever claims a rotation is
//     durable when it is not.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrTeslaTokenRowBusy — the rotation could not take the account's row lock
// within its budget. Declared here at the consumer site (like every other
// interface in this package) and mapped onto by the cmd/ adapter, so
// internal/telemetry never imports internal/store.
var ErrTeslaTokenRowBusy = errors.New("tesla token row busy")

// errNoStoredRefreshToken — the locked row turned out to hold an expired access
// token and no refresh token to spend. Only reachable when the row changed
// between the plain read and the lock; folded into ErrTeslaTokenExpired above.
var errNoStoredRefreshToken = errors.New("no stored refresh token")

// TeslaTokenRotator performs a serialized read-modify-write of one user's
// stored Tesla OAuth pair.
//
// The implementation must call `rotate` while HOLDING an exclusive lock on the
// stored pair, hand it what is stored at that moment, and — when rotate reports
// rotated=true — persist the returned pair before releasing the lock, failing
// the whole call if that write does not land. When rotate reports rotated=false
// nothing is written and the stored pair is returned as-is.
//
// ErrTeslaTokenRowBusy means the lock could not be taken and NOTHING was read or
// spent; the caller is free to look again.
type TeslaTokenRotator interface {
	RotateTeslaToken(
		ctx context.Context,
		userID string,
		rotate func(ctx context.Context, stored TeslaToken) (TeslaToken, bool, error),
	) (TeslaToken, error)
}

// teslaTokenBusyRereadDelay is the pause before the one re-read a contended
// caller performs. Reaching this point already means the rotator waited out its
// whole lock budget, so this is not "wait for the other refresh to finish" — it
// is a beat for a just-committed write to become visible before deciding that a
// contended row genuinely failed to produce a token.
const teslaTokenBusyRereadDelay = 250 * time.Millisecond

// teslaTokenRefresh is the shared on-demand token path. Each surface holds one
// and keeps its own error reporting: the resolver returns sentinels, the two
// HTTP handlers turn those into their own responses.
type teslaTokenRefresh struct {
	tokens    TeslaTokenProvider
	refresher TeslaTokenRefresher // nil disables refresh entirely
	updater   TeslaTokenUpdater   // the unserialized fallback's persistence
	rotator   TeslaTokenRotator   // nil falls back to refresher+updater
	logger    *slog.Logger
}

// resolve returns a usable Tesla token for userID, refreshing an expired one
// through the locked rotation. Errors wrap ErrTeslaTokenUnavailable (nothing on
// file) or ErrTeslaTokenExpired (expired and not refreshable).
func (f teslaTokenRefresh) resolve(ctx context.Context, userID string) (TeslaToken, error) {
	tok, err := f.tokens.GetTeslaToken(ctx, userID)
	if err != nil {
		// Multi-%w (Go 1.20+): errors.Is still matches the sentinel, but the
		// underlying provider error (DB failure, not-found, …) is preserved
		// for logs and for the caller to inspect via errors.As.
		return TeslaToken{}, fmt.Errorf("resolve tesla token: %w: %w", ErrTeslaTokenUnavailable, err)
	}

	// THE FAST PATH. No lock, no transaction — this is every normal request.
	if teslaTokenFresh(tok) {
		return tok, nil
	}

	if f.refresher == nil || tok.RefreshToken == "" {
		return TeslaToken{}, fmt.Errorf("resolve tesla token: %w", ErrTeslaTokenExpired)
	}

	if f.rotator == nil {
		return f.refreshUnserialized(ctx, userID, tok)
	}
	return f.refreshSerialized(ctx, userID)
}

// refreshSerialized rotates under the row lock, double-checking expiry inside it.
func (f teslaTokenRefresh) refreshSerialized(ctx context.Context, userID string) (TeslaToken, error) {
	fresh, err := f.rotator.RotateTeslaToken(ctx, userID,
		func(ctx context.Context, stored TeslaToken) (TeslaToken, bool, error) {
			// THE DOUBLE CHECK. We queued for this lock behind another refresh
			// of the same account; if it succeeded, the token in front of us is
			// its brand-new single-use pair and spending it immediately would
			// recreate the race we just serialized away.
			if stored.AccessToken != "" && teslaTokenFresh(stored) {
				return TeslaToken{}, false, nil
			}
			if stored.RefreshToken == "" {
				return TeslaToken{}, false, errNoStoredRefreshToken
			}

			refreshed, rErr := f.refresher.Refresh(ctx, stored.RefreshToken)
			if rErr != nil {
				// Aborts the transaction with the stored pair untouched: a
				// refusal means the grant is gone, and the row is the only
				// evidence of which account needs re-pairing.
				return TeslaToken{}, false, fmt.Errorf("tesla refresh: %w", rErr)
			}
			return TeslaToken{
				AccessToken:  refreshed.AccessToken,
				RefreshToken: refreshed.RefreshToken,
				ExpiresAt:    refreshed.ExpiresAt(),
			}, true, nil
		})
	if err == nil {
		return fresh, nil
	}

	if errors.Is(err, ErrTeslaTokenRowBusy) {
		// Contention here almost always means another refresh of THIS account
		// is finishing. Look once more before failing a request that somebody
		// else may already have satisfied.
		if tok, ok := f.rereadAfterContention(ctx, userID); ok {
			return tok, nil
		}
	}
	// Multi-%w: keep the sentinel match AND the rotation failure cause.
	return TeslaToken{}, fmt.Errorf("resolve tesla token: refresh failed: %w: %w", ErrTeslaTokenExpired, err)
}

// rereadAfterContention pauses briefly and re-reads the stored token, reporting
// whether it came back usable. Never calls Tesla.
func (f teslaTokenRefresh) rereadAfterContention(ctx context.Context, userID string) (TeslaToken, bool) {
	timer := time.NewTimer(teslaTokenBusyRereadDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return TeslaToken{}, false
	case <-timer.C:
	}

	tok, err := f.tokens.GetTeslaToken(ctx, userID)
	if err != nil || !teslaTokenFresh(tok) {
		return TeslaToken{}, false
	}
	f.logger.Info("tesla token: adopted a concurrent refresh's result",
		slog.String("user_id", userID),
	)
	return tok, true
}

// refreshUnserialized is the pre-MYR-595 behaviour, kept for the wirings that
// have no rotator to hand (unit tests, and any composition root without an
// account repository). It is NOT the production path: it can spend a refresh
// token another caller is also spending, and it still treats a failed persist as
// non-fatal because without the rotation's transaction there is no way to make
// the write and the read atomic.
func (f teslaTokenRefresh) refreshUnserialized(ctx context.Context, userID string, tok TeslaToken) (TeslaToken, error) {
	refreshed, err := f.refresher.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		return TeslaToken{}, fmt.Errorf("resolve tesla token: refresh failed: %w: %w", ErrTeslaTokenExpired, err)
	}

	// Compute expiry once — ExpiresAt() calls time.Now() internally, so reuse
	// the same value for the DB and the in-memory copy.
	expiresAt := refreshed.ExpiresAt()
	if f.updater != nil {
		if uErr := f.updater.UpdateTeslaToken(ctx, userID, refreshed.AccessToken, refreshed.RefreshToken, expiresAt.Unix()); uErr != nil {
			f.logger.Warn("tesla token: failed to persist refreshed token",
				slog.String("user_id", userID),
			)
		}
	}

	return TeslaToken{
		AccessToken:  refreshed.AccessToken,
		RefreshToken: refreshed.RefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

// teslaTokenFresh reports whether a stored token can still be presented to
// Tesla. A zero ExpiresAt means the expiry is UNKNOWN, not elapsed — the same
// reading every copy of this path has always taken.
func teslaTokenFresh(tok TeslaToken) bool {
	return tok.ExpiresAt.IsZero() || !tok.ExpiresAt.Before(time.Now())
}
