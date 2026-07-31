package telemetry

import (
	"context"
	"log/slog"
)

// TeslaGrantRevoker performs the network half of a revocation: hand it a
// refresh token, it tells Tesla the grant is over. Satisfied by *TokenRevoker.
type TeslaGrantRevoker interface {
	Revoke(ctx context.Context, refreshToken string) error
}

// teslaLinkRevoker is the consumer-site seam both severing paths use — the
// account-deletion sequence (whole account) and the per-vehicle teardown
// (last car only). Satisfied by *TeslaLinkRevoker. Nil everywhere it appears
// means "not configured", which is a skip, never a failure.
type teslaLinkRevoker interface {
	RevokeTeslaLink(ctx context.Context, userID string) bool
	RevokeIfLastVehicle(ctx context.Context, userID, vehicleID string) bool
}

// TeslaLinkRevoker actively revokes a user's Tesla OAuth grant BEFORE anything
// deletes the stored tokens (MYR-366).
//
// WHY BEFORE. Once the "Account" row is gone the refresh token is gone with
// it, and the grant can then only be withdrawn by the owner visiting Tesla's
// consent page by hand. Revoking first is the only ordering in which the
// server can do it at all — so the revoke attempt is made while we still hold
// the token, and the deletion then proceeds regardless of how it went.
//
// WHY BEST-EFFORT. The user asked us to delete their data. Tesla being down,
// slow, or already-forgetful is not a reason to refuse them, so every failure
// mode here — no tokens on file, a DB read error, a network error, a Tesla
// 5xx, an already-invalid token — is a WARNING and a false return, never an
// error the caller can propagate. There is no error in the signature on
// purpose: a bool cannot accidentally be returned up a call stack and fail a
// deletion.
//
// RE-RUNNABLE. A second run finds no tokens (the first run's deletion took
// them) and skips cleanly at the GetTeslaToken step, writing no second audit
// line.
//
// It reads the STORED token via TeslaTokenProvider rather than resolving
// through TeslaTokenResolver: revocation needs the refresh token, not a valid
// access token, and resolving would refresh an expired pair — minting a new
// grant from Tesla purely to destroy it, and persisting it to a row that is
// about to be deleted.
type TeslaLinkRevoker struct {
	tokens   TeslaTokenProvider
	vehicles AccountOwnedVehicleLister
	revoker  TeslaGrantRevoker
	logger   *slog.Logger
}

// NewTeslaLinkRevoker wires the revoker. tokens and revoker are required;
// vehicles is only consulted by RevokeIfLastVehicle and may be nil when the
// caller never uses that path.
func NewTeslaLinkRevoker(
	tokens TeslaTokenProvider,
	vehicles AccountOwnedVehicleLister,
	revoker TeslaGrantRevoker,
	logger *slog.Logger,
) *TeslaLinkRevoker {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &TeslaLinkRevoker{tokens: tokens, vehicles: vehicles, revoker: revoker, logger: logger}
}

// RevokeTeslaLink revokes the user's stored Tesla grant. Returns whether Tesla
// accepted the revocation; a false NEVER means the caller should stop.
func (r *TeslaLinkRevoker) RevokeTeslaLink(ctx context.Context, userID string) bool {
	if r == nil || r.tokens == nil || r.revoker == nil {
		return false
	}

	tok, err := r.tokens.GetTeslaToken(ctx, userID)
	if err != nil {
		// No Tesla account row, or the read failed. Either way there is
		// nothing to present to Tesla. Info, not Warn: on the re-run path
		// this is the EXPECTED state, and a warning would train operators to
		// ignore the line.
		r.logger.Info("tesla revocation skipped: no tokens on file",
			slog.String("user_id", userID),
		)
		return false
	}
	if tok.RefreshToken == "" {
		r.logger.Info("tesla revocation skipped: no refresh token on file",
			slog.String("user_id", userID),
		)
		return false
	}

	if err := r.revoker.Revoke(ctx, tok.RefreshToken); err != nil {
		// Non-fatal by design (see the type doc). The stored tokens are
		// deleted anyway, so OUR access ends here either way; what survives
		// is the grant listed on the owner's tesla.com third-party apps page,
		// which the teardown response's revokeUrl still points them at.
		r.logger.Warn("tesla token revocation failed (non-fatal)",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return false
	}

	// Audit line — P0 only: an opaque user cuid and nothing else. NEVER the
	// token, its prefix, its length, or the VIN it was linked to.
	r.logger.Info("tesla_tokens_revoked",
		slog.String("event", "tesla_tokens_revoked"),
		slog.String("user_id", userID),
	)
	return true
}

// RevokeIfLastVehicle revokes the grant only when vehicleID is the owner's
// LAST remaining car — the one case in which store.OwnerTeardown deletes the
// "Account" row. A mid-fleet removal leaves the link (and the other cars'
// telemetry) working, so revoking there would break every car the owner kept.
//
// The last-vehicle test here is a pre-check, deliberately OUTSIDE the
// teardown's `FOR UPDATE` transaction: an HTTP call to Tesla must not be made
// while holding row locks over the owner's fleet. The authoritative
// last-vehicle decision remains the store's, and the handler logs when the two
// disagree — the only way they can is a car added or removed concurrently with
// this teardown, in which case a re-link is the remedy.
func (r *TeslaLinkRevoker) RevokeIfLastVehicle(ctx context.Context, userID, vehicleID string) bool {
	if r == nil || r.vehicles == nil {
		return false
	}

	ids, err := r.vehicles.ListOwnedVehicleIDs(ctx, userID)
	if err != nil {
		r.logger.Warn("tesla revocation skipped: owned-vehicle list failed (non-fatal)",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return false
	}
	if !isOnlyOwnedVehicle(ids, vehicleID) {
		// Mid-fleet removal: the Tesla link stays, so there is nothing to
		// revoke. Silent — this is the common case, not an event.
		return false
	}
	return r.RevokeTeslaLink(ctx, userID)
}

// isOnlyOwnedVehicle reports whether target is the sole member of ids. A set
// that does not contain target at all is NOT a last-vehicle case: the car is
// already gone (or never belonged to this owner), and a teardown that removes
// nothing must not revoke anything.
func isOnlyOwnedVehicle(ids []string, target string) bool {
	return len(ids) == 1 && ids[0] == target
}
