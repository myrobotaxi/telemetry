package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// accountDeletionResult is the internal outcome of a full run.
type accountDeletionResult struct {
	// Counts is the P0 tally handed to the audit row.
	Counts AccountDeletionCounts
	// AlreadyGone is true when the identity transaction found nothing left —
	// a re-run of a completed deletion. Still a 204.
	AlreadyGone bool
}

// accountDeletionError names the step that failed alongside its cause, so the
// server log says where a re-run will resume from without the response body
// leaking the sequence's shape.
type accountDeletionError struct {
	step  string
	cause error
}

// run executes the deletion sequence documented on AccountDeletionHandler.
// Each step is idempotent, so the whole function is safe to call again after
// any failure.
func (h *AccountDeletionHandler) run(ctx context.Context, userID string) (accountDeletionResult, *accountDeletionError) {
	var counts AccountDeletionCounts

	// (1) Drive count for the audit metadata, read BEFORE the teardowns take
	// the drives with them. Deliberately non-fatal: a missing statistic must
	// never block a person's deletion of their own account.
	if n, err := h.deps.Data.CountUserDrives(ctx, userID); err != nil {
		h.logger.Warn("account deletion: drive count failed (non-fatal)",
			slog.String("user_id", userID), slog.String("error", err.Error()))
	} else {
		counts.DriveCount = n
	}

	// (2) Revoke the Tesla OAuth grant at Tesla, while we still hold the
	// refresh token. This MUST precede step 3: the last-vehicle arm of the
	// teardown deletes the "Account" row, and step 9's "User" cascade takes
	// any that survives — after either, the token needed to revoke is gone
	// and only the owner can withdraw the grant by hand. Best-effort and
	// deliberately unchecked: MYR-366 makes Tesla's availability unable to
	// block a person's deletion of their own account.
	h.revokeTeslaLink(ctx, userID)

	// (3) Tear down every owned vehicle through the existing MYR-258
	// transaction — one per car.
	torndown, err := h.tearDownOwnedVehicles(ctx, userID)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "vehicle_teardown", cause: err}
	}
	counts.VehicleCount = torndown

	// (4) Revoke the grants this user REDEEMED. The grants ON their own cars
	// went with step 3; these are the ones pointing the other way.
	revoked, err := h.deps.Data.RevokeSharesReceived(ctx, userID)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "revoke_shares_received", cause: err}
	}
	counts.SharesRevoked = revoked

	// (5) Cancel the open rides this user holds as RIDER, notifying owners.
	cancelled, err := h.cancelOpenRides(ctx, userID)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "cancel_open_rides", cause: err}
	}
	counts.RidesCancelled = cancelled

	// (6) Push devices — the address book goes whole.
	devices, err := h.deps.Data.DeletePushDevices(ctx, userID)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "delete_push_devices", cause: err}
	}
	counts.PushDevicesDeleted = devices

	// (7) Saved places — the person's Home and Work rows (MYR-321). Slotted
	// here, next to the push devices and BEFORE the identity delete, because
	// both are personal effects with no counterparty: rows that belong to this
	// account alone, that no other person has a claim on, and that nothing
	// later in the sequence reads. Deleting them cannot be deferred past step 9
	// — the identity rows go there, and a saved place that outlived its owner
	// would be AES-256-GCM ciphertext of where a deleted person lives, keyed by
	// a cuid nobody can resolve and reachable by nothing but a table scan.
	places, err := h.deps.Data.DeleteSavedPlaces(ctx, userID)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "delete_saved_places", cause: err}
	}
	counts.SavedPlacesDeleted = places

	// (8) Refresh tokens — revoked so no stored session can mint a new access
	// token. The CURRENT access token deliberately keeps working until step 10,
	// because it is what authenticates a re-run if step 9 fails.
	tokens, err := h.deps.Data.RevokeRefreshTokens(ctx, userID)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "revoke_refresh_tokens", cause: err}
	}
	counts.RefreshTokensRevoked = tokens

	// (9) Identity + audit, one transaction, LAST.
	outcome, err := h.deps.Data.DeleteIdentity(ctx, userID, counts)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "delete_identity", cause: err}
	}

	// (10) Close the token window immediately rather than waiting for the
	// existence/access caches to expire on their own.
	h.invalidateSessions(userID)

	return accountDeletionResult{Counts: counts, AlreadyGone: outcome.AlreadyGone}, nil
}

// tearDownOwnedVehicles runs the existing per-vehicle teardown for every car
// the user owns, returning how many were actually removed. A car that is
// already gone counts as done, not as a failure — that is what makes the step
// re-runnable. The FIRST real failure aborts: the remaining cars keep their
// data and a re-run picks them up, which is strictly better than pressing on
// and reporting success over a half-finished teardown.
func (h *AccountDeletionHandler) tearDownOwnedVehicles(ctx context.Context, userID string) (int, error) {
	ids, err := h.deps.Vehicles.ListOwnedVehicleIDs(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("list owned vehicles: %w", err)
	}
	removed := 0
	for _, id := range ids {
		result, err := h.deps.Teardown.RemoveVehicle(ctx, userID, id)
		if err != nil {
			return removed, fmt.Errorf("remove vehicle %s: %w", id, err)
		}
		if result.Removed {
			removed++
		}
	}
	return removed, nil
}

// cancelOpenRides cancels every open ride the user holds as RIDER, through the
// SAME guarded transition the rider-facing cancel endpoint uses, and publishes
// the same ride_status_changed event — so the affected owner is told by the
// standard lifecycle push rather than by a silent disappearance.
//
// Only requested/accepted are cancellable (rideCancellableFrom, rest-api.md
// §7.8). A ride already ENROUTE or ARRIVED is a car physically carrying this
// person right now; cancelling it from under the owner mid-drive would be a
// worse outcome than letting it finish, and it reaches a terminal state on its
// own within the trip. Those rides are LEFT, and after step 9 they render to
// the owner as a former rider exactly as completed history does.
//
// A ride that loses the race (the owner declined or completed it between the
// list and the write) is not an error: the ride is closed either way, which is
// all this step wanted.
func (h *AccountDeletionHandler) cancelOpenRides(ctx context.Context, userID string) (int, error) {
	rides, err := h.deps.Rides.ListOpenRidesByRider(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("list open rides: %w", err)
	}
	cancelled := 0
	for _, ride := range rides {
		if !cancellableFrom(ride.Status) {
			continue
		}
		updated, err := h.deps.Rides.UpdateStatusFrom(ctx, ride.ID, rideCancellableFrom, rideStatusCancelled)
		switch {
		case err == nil:
			cancelled++
			h.publishRideCancelled(ctx, updated)
		case errors.Is(err, ErrRideStatusConflict), errors.Is(err, sdk.ErrNotFound):
			// Someone else closed it first. Nothing left to do.
			h.logger.Info("account deletion: open ride already closed",
				slog.String("user_id", userID),
				slog.String("ride_request_id", ride.ID),
			)
		default:
			return cancelled, fmt.Errorf("cancel ride %s: %w", ride.ID, err)
		}
	}
	return cancelled, nil
}

// publishRideCancelled emits the standard lifecycle event for one cancelled
// ride — the same payload ServeCancel publishes, so the owner's WS frame and
// push notification are byte-identical to a rider tapping Cancel.
func (h *AccountDeletionHandler) publishRideCancelled(ctx context.Context, updated RideRequestData) {
	if h.deps.Events == nil {
		return
	}
	payload := events.RideStatusChangedEvent{
		RideRequestID:    updated.ID,
		VehicleID:        updated.VehicleID,
		RiderID:          updated.RiderID,
		OwnerID:          updated.OwnerID,
		Status:           updated.Status,
		RequesterName:    updated.RequesterName,
		RescheduleStatus: updated.RescheduleStatus,
		ScheduledFor:     updated.ScheduledFor,
		UpdatedAt:        updated.UpdatedAt,
	}
	if err := h.deps.Events.Publish(ctx, events.NewEvent(payload)); err != nil {
		// Non-fatal: the ride IS cancelled and the owner's next read shows it.
		// Failing the deletion here would strand the account over a
		// notification.
		h.logger.Warn("account deletion: publish ride cancellation failed",
			slog.String("ride_request_id", updated.ID),
			slog.String("error", err.Error()),
		)
	}
}

// revokeTeslaLink actively revokes the user's Tesla OAuth grant at Tesla
// before the deletion takes the tokens it needs (MYR-366). Nil deps.TeslaLink
// — no Tesla OAuth client configured — is a skip, and so is a user with no
// Tesla account row, which is both the rider case and the re-run case.
//
// It returns nothing on purpose. There is no failure mode of this step that a
// caller should act on: the account is going either way, and the one thing a
// revocation failure changes is that the grant remains listed on the owner's
// tesla.com third-party-apps page, which they can remove themselves.
func (h *AccountDeletionHandler) revokeTeslaLink(ctx context.Context, userID string) {
	if h.deps.TeslaLink == nil {
		return
	}
	h.deps.TeslaLink.RevokeTeslaLink(ctx, userID)
}

// invalidateSessions drops the auth caches for the deleted user.
func (h *AccountDeletionHandler) invalidateSessions(userID string) {
	if h.deps.Sessions == nil {
		return
	}
	h.deps.Sessions.InvalidateUser(userID)
	h.deps.Sessions.InvalidateVehicles(userID)
}
