package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// TeslaTokenResolver resolves a vehicle owner's Tesla OAuth access token
// OUTSIDE an HTTP request — the same GetTeslaToken + refresh-on-expiry path
// the vehicle-command and fleet-config handlers run, but returning a typed
// error instead of writing an HTTP response. The MYR-176 dispatch pipeline
// needs owner-token resolution with no request context; sharing this type
// keeps the refresh semantics identical across the two surfaces.
//
// The refresher/updater are optional: with no refresher an expired token is a
// hard ErrTeslaTokenExpired (mirrors the handlers, which 401 without one).
type TeslaTokenResolver struct {
	tokens    TeslaTokenProvider
	refresher TeslaTokenRefresher // nil disables auto-refresh
	updater   TeslaTokenUpdater   // nil disables DB persistence of a refresh
	logger    *slog.Logger
}

// TeslaTokenResolverOption configures optional dependencies.
type TeslaTokenResolverOption func(*TeslaTokenResolver)

// WithResolverRefresher enables auto-refresh of an expired token, persisting
// the refreshed set via updater (updater may be nil to skip persistence).
func WithResolverRefresher(refresher TeslaTokenRefresher, updater TeslaTokenUpdater) TeslaTokenResolverOption {
	return func(r *TeslaTokenResolver) {
		r.refresher = refresher
		r.updater = updater
	}
}

// NewTeslaTokenResolver builds a resolver over the given token provider.
func NewTeslaTokenResolver(tokens TeslaTokenProvider, logger *slog.Logger, opts ...TeslaTokenResolverOption) *TeslaTokenResolver {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	r := &TeslaTokenResolver{tokens: tokens, logger: logger}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Token-resolution failure sentinels. Callers use errors.Is to branch;
// messages carry no P1 value (no token material, ids only where opaque).
var (
	// ErrTeslaTokenUnavailable — no Tesla token on file for the user (account
	// not linked, or the lookup failed).
	ErrTeslaTokenUnavailable = errors.New("tesla token unavailable")
	// ErrTeslaTokenExpired — the token is expired and could not be refreshed
	// (no refresher, no refresh token, or the refresh call failed).
	ErrTeslaTokenExpired = errors.New("tesla token expired")
)

// Resolve returns a currently-valid Tesla token for userID, refreshing an
// expired one when a refresher is configured. It never blocks on I/O beyond
// the provider/refresher calls and honors ctx cancellation through them.
func (r *TeslaTokenResolver) Resolve(ctx context.Context, userID string) (TeslaToken, error) {
	tok, err := r.tokens.GetTeslaToken(ctx, userID)
	if err != nil {
		return TeslaToken{}, fmt.Errorf("resolve tesla token: %w", ErrTeslaTokenUnavailable)
	}

	if tok.ExpiresAt.IsZero() || !tok.ExpiresAt.Before(time.Now()) {
		return tok, nil
	}

	if r.refresher == nil || tok.RefreshToken == "" {
		return TeslaToken{}, fmt.Errorf("resolve tesla token: %w", ErrTeslaTokenExpired)
	}

	refreshed, err := r.refresher.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		return TeslaToken{}, fmt.Errorf("resolve tesla token: refresh failed: %w", ErrTeslaTokenExpired)
	}

	expiresAt := refreshed.ExpiresAt()
	if r.updater != nil {
		if uErr := r.updater.UpdateTeslaToken(ctx, userID, refreshed.AccessToken, refreshed.RefreshToken, expiresAt.Unix()); uErr != nil {
			// Non-fatal: we still return the fresh token; the next resolve
			// re-refreshes if persistence keeps failing.
			r.logger.Warn("tesla token resolver: failed to persist refreshed token",
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

// discardWriter drops log output for the nil-logger default.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
