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
// two views of an owner's fleet the deletion machinery needs: ids alone, for
// TeslaLinkRevoker's last-vehicle pre-check, and id+VIN for the deletion
// sequence, which must also stop each car streaming at Tesla (MYR-593).
//
// ONE adapter over ONE repo call, deliberately. Two readers of "the cars this
// owner has" is how the two answers start to differ.
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

// ListOwnedVehicles carries the VIN too. store.Vehicle.VIN is the empty string
// for a car whose Prisma column is NULL (linked but never synced); the deletion
// sequence treats that as "no Tesla-side config to delete" and still tears the
// row down.
func (a *ownedVehicleListerAdapter) ListOwnedVehicles(ctx context.Context, userID string) ([]telemetry.OwnedVehicle, error) {
	vehicles, err := a.repo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.OwnedVehicle, 0, len(vehicles))
	// Indexed: store.Vehicle is a wide snapshot struct and only two fields are read.
	for i := range vehicles {
		out = append(out, telemetry.OwnedVehicle{ID: vehicles[i].ID, VIN: vehicles[i].VIN})
	}
	return out, nil
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

// DeleteProfileNameConfirmation drops the account's display-name confirmation
// row (MYR-583), so no record that this person approved a name outlives the name.
func (a *accountDataDeleterAdapter) DeleteProfileNameConfirmation(ctx context.Context, userID string) (int, error) {
	return a.deleter.DeleteProfileNameConfirmation(ctx, userID)
}

// DeleteUserActivity drops the account's last-seen row (MYR-592, §3.1 step 8c).
func (a *accountDataDeleterAdapter) DeleteUserActivity(ctx context.Context, userID string) (int, error) {
	return a.deleter.DeleteUserActivity(ctx, userID)
}

// DeleteTeslaTokenKeepalive drops the account's keepalive bookkeeping
// (MYR-594, §3.1 step 8d), so no cooldown outlives the account.
func (a *accountDataDeleterAdapter) DeleteTeslaTokenKeepalive(ctx context.Context, userID string) (int, error) {
	return a.deleter.DeleteTeslaTokenKeepalive(ctx, userID)
}

func (a *accountDataDeleterAdapter) RevokeRefreshTokens(ctx context.Context, userID string) (int, error) {
	return a.deleter.RevokeRefreshTokens(ctx, userID)
}

// DeleteRideMemberships drops the account's group-ride memberships (MYR-540),
// so a deleted person cannot linger in a live ride's member list — or in the
// access set that list admits to the ride's vehicle.
func (a *accountDataDeleterAdapter) DeleteRideMemberships(ctx context.Context, userID string) (int, error) {
	return a.deleter.DeleteRideMemberships(ctx, userID)
}

func (a *accountDataDeleterAdapter) DeleteIdentity(ctx context.Context, scope telemetry.AccountDeletionScope, counts telemetry.AccountDeletionCounts) (telemetry.AccountIdentityOutcome, error) {
	res, err := a.deleter.DeleteIdentity(ctx, store.DeletionScope{
		CallerID:    scope.CallerID,
		CanonicalID: scope.CanonicalID,
		IDs:         scope.IDs,
	}, store.AccountDeletionCounts{
		VehicleCount:           counts.VehicleCount,
		DriveCount:             counts.DriveCount,
		RidesCancelled:         counts.RidesCancelled,
		SharesRevoked:          counts.SharesRevoked,
		ShareLabelsScrubbed:    counts.ShareLabelsScrubbed,
		PushDevicesDeleted:     counts.PushDevicesDeleted,
		SavedPlacesDeleted:     counts.SavedPlacesDeleted,
		RideMembershipsDeleted: counts.RideMembershipsDeleted,
		RefreshTokensRevoked:   counts.RefreshTokensRevoked,

		ProfileNameConfirmationsDeleted: counts.ProfileNameConfirmationsDeleted,
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
