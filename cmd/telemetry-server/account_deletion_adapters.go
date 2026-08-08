package main

import (
	"context"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// Adapters for the MYR-355 account-deletion endpoint. Each maps a store type
// onto the narrow consumer-site seam telemetry.AccountDeletionHandler declares,
// at the package boundary, so internal/telemetry never imports internal/store.

// ownedVehicleListerAdapter projects store.VehicleRepo.ListByUser down to the
// vehicle ids the deletion sequence needs.
type ownedVehicleListerAdapter struct {
	repo *store.VehicleRepo
}

func (a *ownedVehicleListerAdapter) ListOwnedVehicleIDs(ctx context.Context, userID string) ([]string, error) {
	vehicles, err := a.repo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(vehicles))
	// Indexed: store.Vehicle is a wide snapshot struct and only the id is read.
	for i := range vehicles {
		ids = append(ids, vehicles[i].ID)
	}
	return ids, nil
}

// accountRideCancellerAdapter reuses the SAME guarded UpdateStatusFrom the
// rider-facing cancel endpoint uses (via the existing rideRequestStoreAdapter,
// so the error mapping — ErrRideStatusConflict / sdk.ErrNotFound — is shared
// rather than re-derived) plus the account-scoped open-ride list.
type accountRideCancellerAdapter struct {
	repo  *store.RideRequestRepo
	rides *rideRequestStoreAdapter
}

func (a *accountRideCancellerAdapter) ListOpenRidesByRider(ctx context.Context, riderID string) ([]telemetry.OpenRideRef, error) {
	refs, err := a.repo.ListOpenByRider(ctx, riderID)
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.OpenRideRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, telemetry.OpenRideRef{ID: ref.ID, Status: string(ref.Status)})
	}
	return out, nil
}

func (a *accountRideCancellerAdapter) UpdateStatusFrom(ctx context.Context, id string, from []string, to string) (telemetry.RideRequestData, error) {
	return a.rides.UpdateStatusFrom(ctx, id, from, to)
}

// accountDataDeleterAdapter maps store.AccountDeleter onto the telemetry-layer
// seam, converting the P0 audit tally at the boundary.
type accountDataDeleterAdapter struct {
	deleter *store.AccountDeleter
}

func (a *accountDataDeleterAdapter) ResolveDeletionScope(ctx context.Context, callerID string) (telemetry.AccountDeletionScope, error) {
	scope, err := a.deleter.ResolveDeletionScope(ctx, callerID)
	if err != nil {
		return telemetry.AccountDeletionScope{}, err
	}
	return telemetry.AccountDeletionScope{
		CallerID:    scope.CallerID,
		CanonicalID: scope.CanonicalID,
		IDs:         scope.IDs,
	}, nil
}

func (a *accountDataDeleterAdapter) CountUserDrives(ctx context.Context, userID string) (int, error) {
	return a.deleter.CountUserDrives(ctx, userID)
}

func (a *accountDataDeleterAdapter) RevokeSharesReceived(ctx context.Context, userID string) (int, error) {
	return a.deleter.RevokeSharesReceived(ctx, userID)
}

func (a *accountDataDeleterAdapter) ScrubSharesReceivedLabel(ctx context.Context, userID string) (int, error) {
	return a.deleter.ScrubSharesReceivedLabel(ctx, userID)
}

func (a *accountDataDeleterAdapter) DeletePushDevices(ctx context.Context, userID string) (int, error) {
	return a.deleter.DeletePushDevices(ctx, userID)
}

func (a *accountDataDeleterAdapter) DeleteSavedPlaces(ctx context.Context, userID string) (int, error) {
	return a.deleter.DeleteSavedPlaces(ctx, userID)
}

func (a *accountDataDeleterAdapter) RevokeRefreshTokens(ctx context.Context, userID string) (int, error) {
	return a.deleter.RevokeRefreshTokens(ctx, userID)
}

func (a *accountDataDeleterAdapter) DeleteIdentity(ctx context.Context, scope telemetry.AccountDeletionScope, counts telemetry.AccountDeletionCounts) (telemetry.AccountIdentityOutcome, error) {
	res, err := a.deleter.DeleteIdentity(ctx, store.DeletionScope{
		CallerID:    scope.CallerID,
		CanonicalID: scope.CanonicalID,
		IDs:         scope.IDs,
	}, store.AccountDeletionCounts{
		VehicleCount:         counts.VehicleCount,
		DriveCount:           counts.DriveCount,
		RidesCancelled:       counts.RidesCancelled,
		SharesRevoked:        counts.SharesRevoked,
		ShareLabelsScrubbed:  counts.ShareLabelsScrubbed,
		PushDevicesDeleted:   counts.PushDevicesDeleted,
		SavedPlacesDeleted:   counts.SavedPlacesDeleted,
		RefreshTokensRevoked: counts.RefreshTokensRevoked,
	})
	if err != nil {
		return telemetry.AccountIdentityOutcome{}, err
	}
	return telemetry.AccountIdentityOutcome{
		Deleted:       res.Deleted,
		AlreadyGone:   res.AlreadyGone,
		HadPrismaUser: res.HadPrismaUser,
		AuditLogID:    res.AuditLogID,
	}, nil
}
