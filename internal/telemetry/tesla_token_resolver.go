package telemetry

import (
	"context"
	"errors"
	"log/slog"
)

// TeslaTokenResolver resolves a vehicle owner's Tesla OAuth access token
// OUTSIDE an HTTP request — the same GetTeslaToken + refresh-on-expiry path
// the vehicle-command and fleet-config handlers run, but returning a typed
// error instead of writing an HTTP response. The MYR-176 dispatch pipeline
// needs owner-token resolution with no request context; sharing this type
// keeps the refresh semantics identical across the two surfaces.
//
// Since MYR-595 all three surfaces share the path itself (teslaTokenRefresh in
// tesla_token_refresh.go) rather than a description of it, so the serialization
// cannot be present on one and missing on another.
//
// The refresher/updater/rotator are optional: with no refresher an expired
// token is a hard ErrTeslaTokenExpired (mirrors the handlers, which 401 without
// one), and with no rotator the refresh runs unserialized.
type TeslaTokenResolver struct {
	tokens    TeslaTokenProvider
	refresher TeslaTokenRefresher // nil disables auto-refresh
	updater   TeslaTokenUpdater   // nil disables DB persistence of a refresh
	rotator   TeslaTokenRotator   // nil disables serialization of a refresh
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

// WithResolverRotator serializes the refresh leg through rotator's row lock
// (MYR-595), which also makes a failed persist fail the resolve instead of
// returning a token that exists nowhere but in memory. Wire it wherever an
// account repository is available; without it the refresh runs the old
// unserialized way.
func WithResolverRotator(rotator TeslaTokenRotator) TeslaTokenResolverOption {
	return func(r *TeslaTokenResolver) { r.rotator = rotator }
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
// expired one when a refresher is configured. A fresh token is answered from a
// plain read; only the refresh leg takes the account's row lock, and it may wait
// a bounded moment for it when another refresh of the same account is in flight.
func (r *TeslaTokenResolver) Resolve(ctx context.Context, userID string) (TeslaToken, error) {
	return teslaTokenRefresh{
		tokens:    r.tokens,
		refresher: r.refresher,
		updater:   r.updater,
		rotator:   r.rotator,
		logger:    r.logger,
	}.resolve(ctx, userID)
}

// discardWriter drops log output for the nil-logger default.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
