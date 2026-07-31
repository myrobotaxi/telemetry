package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// Boundary adapters for the MYR-184 vehicle-sharing surface.
//
// internal/telemetry does not import internal/store, so this is where
// store.VehicleShare rows become telemetry.ShareInviteRow and where the store's
// typed sentinels become the handler layer's. The translation is deliberately
// EXPLICIT rather than a shared error package: the handler layer must not be
// able to accidentally depend on a store-internal error, and each mapping below
// is a documented status decision.

// shareInviteAdapter binds store.VehicleShareRepo to telemetry.ShareInviteStore.
type shareInviteAdapter struct {
	repo *store.VehicleShareRepo
}

// CreateInvite mints an invite and returns the path vehicle's row.
func (a *shareInviteAdapter) CreateInvite(ctx context.Context, in telemetry.ShareInviteCreateInput) (telemetry.ShareInviteRow, error) {
	row, err := a.repo.CreateInvite(ctx, store.CreateShareInviteInput{
		OwnerUserID:   in.OwnerUserID,
		PathVehicleID: in.PathVehicleID,
		VehicleIDs:    in.VehicleIDs,
		Label:         in.Label,
		Permission:    in.Permission,
	})
	if err != nil {
		return telemetry.ShareInviteRow{}, translateShareError(err)
	}
	return toShareInviteRow(&row), nil
}

// ListInvitesForVehicle returns the owner's live rows for one vehicle.
func (a *shareInviteAdapter) ListInvitesForVehicle(ctx context.Context, vehicleID, ownerUserID string) ([]telemetry.ShareInviteRow, error) {
	rows, err := a.repo.ListInvitesForVehicle(ctx, vehicleID, ownerUserID)
	if err != nil {
		return nil, translateShareError(err)
	}
	out := make([]telemetry.ShareInviteRow, 0, len(rows))
	for i := range rows {
		out = append(out, toShareInviteRow(&rows[i]))
	}
	return out, nil
}

// RevokeInvite tombstones a row and reports whose access ended.
func (a *shareInviteAdapter) RevokeInvite(ctx context.Context, inviteID, ownerUserID string) (string, error) {
	viewerID, err := a.repo.RevokeInvite(ctx, inviteID, ownerUserID)
	if err != nil {
		return "", translateShareError(err)
	}
	return viewerID, nil
}

// ResendInvite re-mints the code on a pending row.
func (a *shareInviteAdapter) ResendInvite(ctx context.Context, inviteID, ownerUserID string) (telemetry.ShareInviteRow, error) {
	row, err := a.repo.ResendInvite(ctx, inviteID, ownerUserID)
	if err != nil {
		return telemetry.ShareInviteRow{}, translateShareError(err)
	}
	return toShareInviteRow(&row), nil
}

// OwnerFirstName resolves the calling owner's first name for the `from` half of
// a signed share link (MYR-368). Same repository method the redeem side uses:
// one ladder, one policy, no second definition of "first name" to drift.
func (a *shareInviteAdapter) OwnerFirstName(ctx context.Context, ownerUserID string) (string, error) {
	name, err := a.repo.OwnerFirstName(ctx, ownerUserID)
	if err != nil {
		return "", translateShareError(err)
	}
	return name, nil
}

// shareRedeemAdapter binds store.VehicleShareRepo to telemetry.ShareRedeemStore.
type shareRedeemAdapter struct {
	repo *store.VehicleShareRepo
}

// RedeemCode accepts every pending row for a code atomically.
func (a *shareRedeemAdapter) RedeemCode(ctx context.Context, code, redeemerID string) ([]telemetry.ShareGrantRow, error) {
	grants, err := a.repo.RedeemCode(ctx, code, redeemerID)
	if err != nil {
		return nil, translateShareError(err)
	}
	out := make([]telemetry.ShareGrantRow, 0, len(grants))
	for _, g := range grants {
		out = append(out, telemetry.ShareGrantRow{
			VehicleID:   g.VehicleID,
			OwnerUserID: g.OwnerUserID,
			Permission:  g.Permission,
		})
	}
	return out, nil
}

// OwnerFirstName resolves the sharing owner's first name.
func (a *shareRedeemAdapter) OwnerFirstName(ctx context.Context, ownerUserID string) (string, error) {
	return a.repo.OwnerFirstName(ctx, ownerUserID)
}

// shareReaderAdapter binds store.VehicleShareRepo to
// telemetry.VehicleShareReader — the per-vehicle tier lookup every read gate
// consults for a non-owner.
type shareReaderAdapter struct {
	repo *store.VehicleShareRepo
}

// SharePermissionFor resolves the caller's tier over one vehicle. The store's
// ErrShareNotFound already wraps sdk.ErrNotFound, which is the "no grant"
// signal the gate checks, so it passes through untranslated.
func (a *shareReaderAdapter) SharePermissionFor(ctx context.Context, userID, vehicleID string) (auth.SharePermission, error) {
	permission, err := a.repo.SharePermissionFor(ctx, userID, vehicleID)
	if err != nil {
		return auth.SharePermission(""), err
	}
	// Parse rather than cast: a tier the enum does not recognize must not
	// reach a gate, where AtLeast would silently refuse it while the caller
	// believed a grant existed. A parse failure is a real error (a row whose
	// CHECK constraint has drifted), not a denial.
	tier, err := auth.ParseSharePermission(permission)
	if err != nil {
		return auth.SharePermission(""), fmt.Errorf("shareReaderAdapter(vehicle=%s): %w", vehicleID, err)
	}
	return tier, nil
}

// sharedVehicleListerAdapter binds the viewer-side catalog reads to
// telemetry.SharedVehicleLister.
type sharedVehicleListerAdapter struct {
	repo *store.VehicleRepo
}

// ListSharedByUser returns every vehicle shared with the caller.
func (a *sharedVehicleListerAdapter) ListSharedByUser(ctx context.Context, userID string) ([]telemetry.SharedVehicleRow, error) {
	rows, err := a.repo.ListSharedSummariesByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return toSharedVehicleRows(rows), nil
}

// ListSharedByIDs narrows the same access-checked query to one redemption's set.
func (a *sharedVehicleListerAdapter) ListSharedByIDs(ctx context.Context, userID string, vehicleIDs []string) ([]telemetry.SharedVehicleRow, error) {
	rows, err := a.repo.ListSharedSummariesByIDs(ctx, userID, vehicleIDs)
	if err != nil {
		return nil, err
	}
	return toSharedVehicleRows(rows), nil
}

// toSharedVehicleRows converts store rows to the handler's catalog shape.
func toSharedVehicleRows(rows []store.SharedVehicleSummary) []telemetry.SharedVehicleRow {
	out := make([]telemetry.SharedVehicleRow, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		out = append(out, telemetry.SharedVehicleRow{
			VehicleCatalogRow: telemetry.VehicleCatalogRow{
				ID:                   row.ID,
				VIN:                  row.VIN,
				Name:                 row.Name,
				Model:                row.Model,
				Year:                 row.Year,
				Color:                row.Color,
				LicensePlate:         row.LicensePlate,
				Status:               string(row.Status),
				ChargeLevel:          row.ChargeLevel,
				EstimatedRange:       row.EstimatedRange,
				LastUpdated:          row.LastUpdated,
				HasActiveRide:        row.HasActiveRide,
				ServiceETC:           row.ServiceETC,
				ServiceExpectedEndAt: row.ServiceExpectedEndAt,
				// MYR-342: viewers see the pause too — the whole point is that a
				// rider learns the shared car is not taking requests from the
				// catalog, not from a 409.
				RideShareEnabled: row.RideShareEnabled,
			},
			Permission: row.Permission,
		})
	}
	return out
}

// toShareInviteRow drops the two server-only columns (accepted_by_user_id,
// revoked_at) that the owner-facing wire shape deliberately never carries.
func toShareInviteRow(row *store.VehicleShare) telemetry.ShareInviteRow {
	return telemetry.ShareInviteRow{
		ID:         row.ID,
		VehicleID:  row.VehicleID,
		Label:      row.Label,
		Permission: row.Permission,
		Code:       row.Code,
		Status:     row.Status,
		CreatedAt:  row.CreatedAt,
		ExpiresAt:  row.ExpiresAt,
		AcceptedAt: row.AcceptedAt,
	}
}

// translateShareError maps store sentinels onto the handler layer's.
//
// store.ErrShareNotFound is NOT listed: it already wraps sdk.ErrNotFound, which
// is the signal the handlers check, and re-wrapping it would only create a
// second thing to keep in sync. Everything unrecognized passes through and
// becomes a 500 — the safe default for an error nobody has classified.
func translateShareError(err error) error {
	switch {
	case errors.Is(err, store.ErrShareVehicleNotOwned):
		return telemetry.ErrShareVehicleNotOwned
	case errors.Is(err, store.ErrShareNotPending):
		return telemetry.ErrShareNotPending
	case errors.Is(err, store.ErrShareSelfRedeem):
		return telemetry.ErrShareSelfRedeem
	case errors.Is(err, store.ErrShareAlreadyGranted):
		return telemetry.ErrShareAlreadyGranted
	default:
		return err
	}
}
