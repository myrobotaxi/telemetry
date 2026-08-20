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
	// StreamConfigsDeleted counts the Tesla-side telemetry configs this run
	// actually removed (MYR-593). Deliberately NOT in Counts: Counts is
	// mirrored field-for-field into the store's audit metadata, and this is an
	// operational fact about a best-effort third-party call, not a statement
	// about what was erased. It reaches the completion log line and stops there.
	StreamConfigsDeleted int
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

	// (0) Resolve the caller's JWT subject to the FULL set of ids that make up
	// this person, before anything is keyed on it (MYR-452).
	//
	// This step exists because the subject is not trustworthy as the account's
	// key. A Tesla link that converges two identities re-points the Apple
	// binding onto a canonical id and abandons the caller's original one, and
	// nothing re-issues the caller's tokens — so a converged owner keeps
	// presenting the OLD id for the life of their refresh family. Every step
	// below used to take that id at face value, which meant the teardown
	// revoked nothing, deleted nothing, wrote its audit row and answered 204
	// while the account stood untouched. The surviving binding then signed the
	// person straight back in as Owner on their next Sign in with Apple.
	//
	// It is fatal on error, unlike the drive count below: proceeding on a
	// half-resolved scope is how a deletion silently misses its target, and
	// that is the exact failure this step was added to prevent.
	scope, err := h.deps.Data.ResolveDeletionScope(ctx, userID)
	if err != nil {
		// A dedicated, greppable event. A refusal here means a person cannot
		// delete their own account until an operator repairs the convergence
		// graph, which is a privacy-commitment failure and must page someone
		// rather than hide inside the generic 500 the caller sees.
		h.logger.Error("account_deletion_scope_unresolved",
			slog.String("event", "account_deletion_scope_unresolved"),
			slog.String("caller_id", userID),
			slog.String("error", err.Error()))
		return accountDeletionResult{}, &accountDeletionError{step: "resolve_identity_scope", cause: err}
	}
	if scope.Converged() {
		h.logger.Info("account deletion: caller authenticated under a converged identity",
			slog.String("caller_id", scope.CallerID),
			slog.String("user_id", scope.CanonicalID),
			slog.Int("scope_size", len(scope.IDs)))
	}

	// (1) Drive count for the audit metadata, read BEFORE the teardowns take
	// the drives with them. Deliberately non-fatal: a missing statistic must
	// never block a person's deletion of their own account.
	if n, err := h.sumOverScope(ctx, scope, h.deps.Data.CountUserDrives); err != nil {
		h.logger.Warn("account deletion: drive count failed (non-fatal)",
			slog.String("user_id", scope.CanonicalID), slog.String("error", err.Error()))
	} else {
		counts.DriveCount = n
	}

	// (1b) Read the account's whole fleet, ONCE. Both of the next two steps
	// operate on it — the Tesla-side config delete and the row teardown — and
	// they must agree on which cars those are. Fatal on error for the same
	// reason step 3 always was: a fleet we could not read is a fleet we cannot
	// prove we finished, and a re-run costs one query.
	owned, err := h.listOwnedVehicles(ctx, scope)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "list_owned_vehicles", cause: err}
	}

	// (1c) Stop every one of those cars streaming at Tesla (MYR-593).
	//
	// THIS IS THE FIRST TESLA CALL OF THE SEQUENCE, AND IT HAS TO BE. Two later
	// steps destroy what authenticates it, in this order: step 2 revokes the
	// OAuth GRANT at Tesla — after which the access token is dead at the far end
	// however fresh our copy looks — and step 3's last-vehicle arm deletes the
	// "Account" row the token is stored in. There is no position after either
	// one from which this call can succeed, so it goes before both.
	//
	// WHAT IT COSTS TO SKIP, which is why it is here at all. The fleet config
	// lives at Tesla with a 350-day `exp`, and nothing local expires it. Delete
	// the Vehicle row without it and the car keeps streaming and keeps billing
	// for the best part of a year, unreachable: the MYR-592 inactivity sweeper
	// joins from a live Vehicle row and there is no longer one, and the owner
	// who could revoke it by hand no longer has an account. This step is the
	// only moment in the system's life when that config can be removed.
	//
	// Best-effort per car, like every other Tesla call in this sequence. A car
	// that is unreachable, a token already dead, a 404 for a config that was
	// never applied — each is logged and the next car is tried.
	streamConfigsDeleted := h.deleteStreamConfigs(ctx, owned)

	// (2) Revoke the Tesla OAuth grant at Tesla, while we still hold the
	// refresh token. This MUST precede step 3: the last-vehicle arm of the
	// teardown deletes the "Account" row, and step 10's "User" cascade takes
	// any that survives — after either, the token needed to revoke is gone
	// and only the owner can withdraw the grant by hand. Best-effort and
	// deliberately unchecked: MYR-366 makes Tesla's availability unable to
	// block a person's deletion of their own account.
	h.revokeTeslaLink(ctx, scope)

	// (3) Tear down every owned vehicle through the existing MYR-258
	// transaction — one per car.
	torndown, err := h.tearDownOwnedVehicles(ctx, owned)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "vehicle_teardown", cause: err}
	}
	counts.VehicleCount = torndown

	// (4) Revoke the grants this user REDEEMED. The grants ON their own cars
	// went with step 3; these are the ones pointing the other way.
	revoked, err := h.sumOverScope(ctx, scope, h.deps.Data.RevokeSharesReceived)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "revoke_shares_received", cause: err}
	}
	counts.SharesRevoked = revoked

	// (5) Erase the owner-typed label from those same redeemed grants (MYR-447).
	// Step 4 tombstones the ACCESS; this erases the NAME. They are separate
	// steps because they cover different rows: step 4 is guarded by
	// `status <> 'revoked'` and so deliberately skips grants that were already
	// revoked, whereas the name on those grants is exactly as stale and exactly
	// as much this person's PII. Keyed on the person, not the status.
	scrubbed, err := h.sumOverScope(ctx, scope, h.deps.Data.ScrubSharesReceivedLabel)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "scrub_share_labels", cause: err}
	}
	counts.ShareLabelsScrubbed = scrubbed

	// (6) Cancel the open rides this user holds as RIDER, notifying owners.
	cancelled, err := h.cancelOpenRides(ctx, scope)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "cancel_open_rides", cause: err}
	}
	counts.RidesCancelled = cancelled

	// (6b) Group-ride MEMBERSHIPS (MYR-540) — the same fact from the other side.
	// Step 6 ends the rides this person BOOKED; this ends the rides they merely
	// JOINED, which belong to somebody else and are still running.
	//
	// It is a DELETE and it has to be, because the row is not inert: while it
	// stands, the deleted account is a name in a live ride's `members` array and
	// — the part that matters — an entry in the access set that admits a
	// WebSocket to that ride's vehicle. Leaving it for the FK cascade would work
	// only in the direction that does not need help: the cascade fires when the
	// RIDE goes, and the ride here is not going anywhere.
	memberships, err := h.sumOverScope(ctx, scope, h.deps.Data.DeleteRideMemberships)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "delete_ride_memberships", cause: err}
	}
	counts.RideMembershipsDeleted = memberships

	// (7)-(9) The personal effects, in their own helper — see runPersonalEffects
	// for what "personal effects" means and why the three sit together.
	if derr := h.runPersonalEffects(ctx, scope, &counts); derr != nil {
		return accountDeletionResult{}, derr
	}

	// (10) Identity + audit, one transaction, LAST. Scope-wide: this is the
	// statement that takes the Apple binding, and a binding left standing is
	// the whole of MYR-452 — Apple returns the same `sub` forever, so any
	// surviving row keyed to it re-recognises the person on their next sign-in.
	outcome, err := h.deps.Data.DeleteIdentity(ctx, scope, counts)
	if err != nil {
		return accountDeletionResult{}, &accountDeletionError{step: "delete_identity", cause: err}
	}

	// (11) Close the token window immediately rather than waiting for the
	// existence/access caches to expire on their own.
	h.invalidateSessions(scope)

	return accountDeletionResult{
		Counts:               counts,
		AlreadyGone:          outcome.AlreadyGone,
		StreamConfigsDeleted: streamConfigsDeleted,
	}, nil
}

// sumOverScope runs one user-scoped step across every id in the closure and
// returns the total rows it affected, stopping at the first error.
//
// Running the same statement two or three times with different ids is safe by
// construction: every step in this sequence is idempotent and keyed only by
// user id, which is exactly the property that already makes the whole endpoint
// re-runnable after a mid-failure. In the un-converged case — very nearly all of
// them — the closure holds a single id and this is one call, unchanged.
func (h *AccountDeletionHandler) sumOverScope(
	ctx context.Context,
	scope AccountDeletionScope,
	step func(context.Context, string) (int, error),
) (int, error) {
	total := 0
	for _, id := range scope.IDs {
		n, err := step(ctx, id)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// cancelOpenRides cancels every open ride the user holds as RIDER, through the
// SAME guarded transition the rider-facing cancel endpoint uses, and publishes
// the same ride_status_changed event — so the affected owner is told by the
// standard lifecycle push rather than by a silent disappearance.
//
// Only requested/accepted are cancelled here — accountDeletionCancellableFrom,
// DELIBERATELY NARROWER than the rider's own interactive window since MYR-537
// widened that to every live status. The rider's mid-ride cancel is a person
// aboard making a deliberate choice the owner is pushed about; a DELETION is
// bookkeeping, and a ride already ENROUTE or ARRIVED is a car physically
// carrying this person right now — cancelling it from under the owner
// mid-drive would be a worse outcome than letting it finish, and it reaches a
// terminal state on its own within the trip. Those rides are LEFT, and after
// step 10 they render to the owner as a former rider exactly as completed
// history does.
//
// A ride that loses the race (the owner declined or completed it between the
// list and the write) is not an error: the ride is closed either way, which is
// all this step wanted.
func (h *AccountDeletionHandler) cancelOpenRides(ctx context.Context, scope AccountDeletionScope) (int, error) {
	cancelled := 0
	for _, riderID := range scope.IDs {
		rides, err := h.deps.Rides.ListOpenRidesByRider(ctx, riderID)
		if err != nil {
			return cancelled, fmt.Errorf("list open rides: %w", err)
		}
		for _, ride := range rides {
			if !accountDeletionCancellable(ride.Status) {
				continue
			}
			updated, err := h.deps.Rides.UpdateStatusFrom(ctx, ride.ID, accountDeletionCancellableFrom, rideStatusCancelled)
			switch {
			case err == nil:
				cancelled++
				h.publishRideCancelled(ctx, updated)
			case errors.Is(err, ErrRideStatusConflict), errors.Is(err, sdk.ErrNotFound):
				// Someone else closed it first. Nothing left to do.
				h.logger.Info("account deletion: open ride already closed",
					slog.String("user_id", riderID),
					slog.String("ride_request_id", ride.ID),
				)
			default:
				return cancelled, fmt.Errorf("cancel ride %s: %w", ride.ID, err)
			}
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
		// MYR-548: the frame's refetch signal, carried on every lifecycle
		// publish. See mutateStatusWith for why a transition that does not BUMP
		// the version must still carry it.
		TripVersion: updated.TripVersion,
		UpdatedAt:   updated.UpdatedAt,
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
func (h *AccountDeletionHandler) revokeTeslaLink(ctx context.Context, scope AccountDeletionScope) {
	if h.deps.TeslaLink == nil {
		return
	}
	// Per id in the closure: the "Account" row holding the refresh token this
	// call needs is filed under the canonical id, which a converged caller's
	// token does not name.
	for _, id := range scope.IDs {
		h.deps.TeslaLink.RevokeTeslaLink(ctx, id)
	}
}

// invalidateSessions drops the auth caches for every id the deleted user
// authenticated under — the caller's own subject included, which is the one
// their still-unexpired access token actually presents.
func (h *AccountDeletionHandler) invalidateSessions(scope AccountDeletionScope) {
	if h.deps.Sessions == nil {
		return
	}
	for _, id := range scope.IDs {
		h.deps.Sessions.InvalidateUser(id)
		h.deps.Sessions.InvalidateVehicles(id)
	}
}

// accountDeletionCancellableFrom is the deletion teardown's own allowed-from
// set — the pre-MYR-537 pair, kept on purpose (see cancelOpenRides). It must
// NOT track rideCancellableFrom: the two windows parted ways the day the
// rider's grew arrived/enroute.
var accountDeletionCancellableFrom = []string{rideStatusRequested, rideStatusAccepted}

func accountDeletionCancellable(status string) bool {
	return status == rideStatusRequested || status == rideStatusAccepted
}

// runPersonalEffects runs the steps whose rows belong to THIS ACCOUNT ALONE and
// that no other person has a claim on: the APNs address book, the saved places,
// the display-name confirmation (MYR-583) and the refresh-token family. Split out
// of run (MYR-540 pushed it past the 80-line cap) rather than inlined, because
// they share one argument and it is worth stating once.
//
// All three run BEFORE the identity delete, and their order among themselves is
// unconstrained — nothing later in the sequence reads any of them, and no
// teardown, cascade or event depends on them. What IS constrained is that they
// precede step 10: a saved place that outlived its owner would be AES-256-GCM
// ciphertext of where a deleted person lives, keyed by a cuid nothing can
// resolve and reachable by nothing but a table scan.
//
// counts is mutated in place; the caller hands the same tally to DeleteIdentity.
func (h *AccountDeletionHandler) runPersonalEffects(
	ctx context.Context, scope AccountDeletionScope, counts *AccountDeletionCounts,
) *accountDeletionError {
	// (7) Push devices — the address book goes whole.
	devices, err := h.sumOverScope(ctx, scope, h.deps.Data.DeletePushDevices)
	if err != nil {
		return &accountDeletionError{step: "delete_push_devices", cause: err}
	}
	counts.PushDevicesDeleted = devices

	// (8) Saved places — the person's Home and Work rows (MYR-321). Slotted
	// here, next to the push devices and BEFORE the identity delete, because
	// both are personal effects with no counterparty: rows that belong to this
	// account alone, that no other person has a claim on, and that nothing
	// later in the sequence reads. Deleting them cannot be deferred past step 10
	// — the identity rows go there, and a saved place that outlived its owner
	// would be AES-256-GCM ciphertext of where a deleted person lives, keyed by
	// a cuid nobody can resolve and reachable by nothing but a table scan.
	places, err := h.sumOverScope(ctx, scope, h.deps.Data.DeleteSavedPlaces)
	if err != nil {
		return &accountDeletionError{step: "delete_saved_places", cause: err}
	}
	counts.SavedPlacesDeleted = places

	// (8b) The display-name confirmation row (MYR-583) — the record that this
	// person approved the name the platform showed other people. A personal
	// effect like the two above it: keyed on the person alone, read by nothing
	// later in the sequence, and claimed by no counterparty.
	//
	// It is a separate step from the identity delete on purpose, even though it is
	// a consent record about a name. It names no identity rung and authenticates
	// nothing, so putting it in step 10's transaction would grow the one
	// transaction that must stay small and lock-ordered for a row that no other
	// step depends on. Left behind, it would be a standing assertion that a person
	// who no longer exists approved a name that no longer exists — P0, so not a
	// leak, but exactly the kind of orphan §1.4.2 exists to forbid.
	confirmations, err := h.sumOverScope(ctx, scope, h.deps.Data.DeleteProfileNameConfirmation)
	if err != nil {
		return &accountDeletionError{step: "delete_profile_name_confirmation", cause: err}
	}
	counts.ProfileNameConfirmationsDeleted = confirmations

	// (8c) The last-seen row (MYR-592). Grouped with the personal effects for
	// the same reasons as 8b — keyed only by user_id, read by nothing later in
	// the sequence — but it is P1 rather than P0, so removing it is an erasure
	// obligation and not only hygiene: it is a behavioural record of when this
	// person was last using the product. It also stops the inactivity sweeper
	// believing in an account that no longer exists.
	activity, err := h.sumOverScope(ctx, scope, h.deps.Data.DeleteUserActivity)
	if err != nil {
		return &accountDeletionError{step: "delete_user_activity", cause: err}
	}
	counts.UserActivityRowsDeleted = activity

	// (9) Refresh tokens — revoked so no stored session can mint a new access
	// token. The CURRENT access token deliberately keeps working until step 11,
	// because it is what authenticates a re-run if step 10 fails.
	tokens, err := h.sumOverScope(ctx, scope, h.deps.Data.RevokeRefreshTokens)
	if err != nil {
		return &accountDeletionError{step: "revoke_refresh_tokens", cause: err}
	}
	counts.RefreshTokensRevoked = tokens

	return nil
}
