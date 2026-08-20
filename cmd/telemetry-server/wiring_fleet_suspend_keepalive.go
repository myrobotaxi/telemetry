package main

// The MYR-594 KEEPALIVE arm’s wiring, split from wiring_fleet_suspend.go to keep
// both files inside the 300-line cap. Everything here serves one job: stopping a
// suspended owner’s Tesla refresh token from lapsing while they are away.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/fleetsuspend"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

const (

	// fleetSuspendKeepaliveAfter is how stale a suspended owner's Tesla grant
	// may get before it is rotated (MYR-594). Tesla lapses an unused refresh
	// token at roughly ninety days; forty-five is half of that, which leaves a
	// fortnight of failed passes before anything is genuinely at risk while
	// still meaning a dormant account is touched about eight times a year.
	fleetSuspendKeepaliveAfter = 45 * 24 * time.Hour

	// fleetSuspendKeepaliveRetryAfter is the minimum gap between two attempts at
	// the same owner. It is the rotation-loop brake: a rotation whose STORE
	// failed leaves the grant's expiry unmoved, so without this the next pass
	// would spend another single-use token on the same broken write.
	fleetSuspendKeepaliveRetryAfter = 24 * time.Hour

	// fleetSuspendKeepaliveCooldown is how long a refusal suppresses further
	// attempts. A grant Tesla has refused is lapsed or revoked and no retry
	// cures either, so this is deliberately long; the store releases it earlier
	// if anything rotates the grant in the meantime.
	fleetSuspendKeepaliveCooldown = 7 * 24 * time.Hour

	// fleetSuspendKeepaliveMaxPerSweep caps the rotations one pass performs, and
	// so caps the signed calls this arm can make against Tesla's OAuth endpoint
	// in an hour. Much tighter than the suspend cap above because the upstream
	// is different in kind: being rate-limited on the OAuth endpoint costs
	// credentials, not latency. Hourly passes drain a backlog quickly.
	fleetSuspendKeepaliveMaxPerSweep = 20

	// fleetSuspendKeepaliveTimeout bounds one owner's rotation — the Tesla call
	// plus the writes it happens between, all of it holding a row lock on the
	// owner's OAuth row. It is the ceiling on how long a concurrent reconnect
	// could be made to wait, so it is kept to the refresher's own budget.
	fleetSuspendKeepaliveTimeout = 30 * time.Second
)

// keepaliveOption wires the MYR-594 arm, or returns a no-op when it cannot run.
//
// IT NEEDS THE OAUTH CLIENT CREDENTIALS AND NOTHING ELSE FROM THE PROXY. That
// is the one wiring difference from the suspend arm above, and it is not an
// oversight: suspension calls the Fleet API through the tesla-http-proxy, while
// a token rotation is a plain POST to Tesla's OAuth endpoint. A deployment with
// credentials but no proxy therefore keeps its dormant owners' grants alive
// while suspending nothing, which is the correct half to keep.
func keepaliveOption(deps httpRouteDeps, log *slog.Logger) fleetsuspend.SweeperOption {
	if deps.cfg.TeslaOAuth().ClientID == "" {
		// No credentials, no rotation. WithKeepalive is not called at all, so
		// the arm does not exist rather than existing and failing every pass.
		return func(*fleetsuspend.Sweeper) {}
	}
	sub := log.With(slog.String("subcomponent", "token-keepalive"))
	return fleetsuspend.WithKeepalive(
		&fleetSuspendKeepaliveStore{repo: store.NewTeslaTokenKeepaliveRepo(deps.pool)},
		&fleetSuspendRotator{
			accounts: deps.accountRepo,
			refresher: telemetry.NewTokenRefresher(telemetry.TeslaOAuthConfig{
				ClientID:     deps.cfg.TeslaOAuth().ClientID,
				ClientSecret: deps.cfg.TeslaOAuth().ClientSecret,
			}, sub),
		},
	)
}

// fleetSuspendKeepaliveStore re-types the keepalive repo's rows onto the
// sweeper's own shapes, the same seam fleetSuspendStoreAdapter provides for the
// warn/suspend arms.
type fleetSuspendKeepaliveStore struct {
	repo *store.TeslaTokenKeepaliveRepo
}

func (a *fleetSuspendKeepaliveStore) ListStaleTokenOwners(
	ctx context.Context, q fleetsuspend.KeepaliveQuery,
) ([]fleetsuspend.KeepaliveCandidate, error) {
	rows, err := a.repo.ListStaleTokenOwners(ctx, store.StaleTokenOwnerQuery{
		StaleBefore:   q.StaleBefore,
		QuietBefore:   q.QuietBefore,
		AttemptBefore: q.AttemptBefore,
		FailureBefore: q.FailureBefore,
		Limit:         q.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]fleetsuspend.KeepaliveCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, fleetsuspend.KeepaliveCandidate{
			OwnerID:         r.OwnerID,
			TokenExpiresAt:  r.TokenExpiresAt,
			TokenExpiryUnix: r.TokenExpiryUnix,
		})
	}
	return out, nil
}

func (a *fleetSuspendKeepaliveStore) RecordKeepaliveSuccess(ctx context.Context, userID string, at time.Time) error {
	return a.repo.RecordKeepaliveSuccess(ctx, userID, at)
}

func (a *fleetSuspendKeepaliveStore) RecordKeepaliveFailure(
	ctx context.Context, userID string, at time.Time, tokenExpiryUnix int64,
) error {
	return a.repo.RecordKeepaliveFailure(ctx, userID, at, tokenExpiryUnix)
}

// fleetSuspendRotator is the keepalive arm's rotation, and it is deliberately
// NOT the TeslaTokenResolver the arms above use.
//
// The resolver is the on-demand path: it reads the stored token with a plain
// SELECT, refreshes only if the ACCESS token has already expired, and — the
// part that rules it out here — treats a failure to persist the rotated pair as
// non-fatal, logging it and handing the caller a token that exists nowhere but
// in memory. For a user's in-flight request that is the right trade: they get
// their answer and the next request re-refreshes. For a background arm whose
// entire purpose is that the STORED pair stays usable, silently discarding the
// only copy of a single-use rotation is the exact failure it exists to prevent.
//
// So it goes through store.RotateTeslaTokenLocked instead, which wraps the SAME
// refresher and the SAME update statement in a transaction over a
// `FOR UPDATE NOWAIT` row lock and returns every error, including the store's.
type fleetSuspendRotator struct {
	accounts  *store.AccountRepo
	refresher *telemetry.TokenRefresher
}

func (a *fleetSuspendRotator) RotateTeslaToken(ctx context.Context, userID string) error {
	err := a.accounts.RotateTeslaTokenLocked(ctx, userID,
		func(ctx context.Context, refreshToken string) (store.TeslaTokenPair, error) {
			refreshed, rErr := a.refresher.Refresh(ctx, refreshToken)
			if rErr != nil {
				return store.TeslaTokenPair{}, rErr
			}
			return store.TeslaTokenPair{
				AccessToken:  refreshed.AccessToken,
				RefreshToken: refreshed.RefreshToken,
				ExpiresAt:    refreshed.ExpiresAt().Unix(),
			}, nil
		})
	if errors.Is(err, store.ErrTeslaTokenRowBusy) {
		// Translated at the boundary so internal/fleetsuspend can branch on
		// "somebody else is rotating this" without importing internal/store.
		return fmt.Errorf("keepalive rotate(user=%s): %w", userID, fleetsuspend.ErrRotationBusy)
	}
	return err
}
