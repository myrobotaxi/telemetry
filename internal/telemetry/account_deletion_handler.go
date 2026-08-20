package telemetry

import (
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// AccountDeletionHandler serves DELETE /api/users/me — user-initiated account
// deletion (MYR-355; FR-10.1/FR-10.2; rest-api.md §7.6; data-lifecycle.md §3).
// It is an App Store review requirement: an app that creates accounts must
// offer in-app account deletion.
//
// WHO SERVES THIS. rest-api.md §7.6 originally assigned the endpoint to the
// Next.js app, on the reading that the Prisma `$transaction` was the deletion.
// The native iOS client (P9) never talks to the Next.js app at all, and the
// data that must go now lives largely in Go-owned tables the Prisma cascade
// cannot reach (go_ride_requests, go_vehicle_shares, go_push_devices,
// go_refresh_tokens, go_users, go_identity_apple, go_removed_vehicles). So the
// Go server serves it, and the §3.1 `account_deleted` audit row moves with it.
//
// THE SEQUENCE, and why it is not one transaction. See
// store.AccountDeleter's type doc for the full reasoning; the short form is
// that the per-vehicle teardown is ALREADY its own transaction (it takes FOR
// UPDATE locks and fires a NOTIFY that must not observe uncommitted work) and
// the ride cancellation publishes push notifications. Instead of one-shot
// atomicity the endpoint guarantees that every step is idempotent and the
// whole sequence is RE-RUNNABLE:
//
//  1. Count drives (audit metadata only; a failure here is not fatal).
//  2. Revoke the Tesla OAuth grant AT TESLA (MYR-366), while the stored
//     refresh token still exists to present. Best-effort and non-fatal:
//     Tesla's availability must not be able to block a person's deletion of
//     their own account.
//  3. Tear down every owned vehicle, one existing owner_teardown transaction
//     per car. Each removes the Vehicle + cascade, deletes that car's
//     go_ride_requests, revokes every sharing grant ON the car (MYR-184),
//     tombstones the VIN (MYR-261) and, on the last car, clears the Tesla
//     Account tokens + resets Settings. Idempotent: AlreadyGone is success.
//  4. Revoke the grants the caller RECEIVED (shares-received → tombstones).
//  5. Scrub the owner-typed label off those same received grants (MYR-447).
//     Step 4 tombstones the ACCESS but leaves the NAME: `label` is a third
//     party's free-text name for this person, typed by the car owner, and
//     before MYR-447 it survived the deletion indefinitely. Keyed on the
//     person rather than the status, so it also reaches grants that were
//     already revoked — which step 4, being idempotency-guarded, skips.
//  6. Cancel the caller's OPEN rides as RIDER, through the guarded status
//     transition, publishing ride_status_changed so the affected owners are
//     notified by the standard lifecycle pushes.
//  7. Delete the caller's push devices.
//  8. Delete the caller's saved Home/Work places (MYR-321) — personal effects
//     with no counterparty, and the one step whose rows are encrypted GPS of
//     where the person lives, so they must not outlive the account.
//  9. Revoke the caller's refresh tokens.
//  10. Delete the identity rows (go_identity_apple, go_users) and the Prisma
//     "User" row if one exists — ONE transaction, with the account_deleted
//     audit row written first (CG-DL-3).
//  11. Invalidate the auth caches so the caller's unexpired access token stops
//     validating immediately.
//
// The order is load-bearing in one specific way: the identity rows the caller
// authenticates WITH are deleted LAST. A failure at any earlier step therefore
// leaves an account that can still call this endpoint again and finish the
// job — which is exactly what the client does on the retry path.
//
// RIDE HISTORY IS NOT DELETED. Terminal ride rows (completed / declined /
// cancelled) where the caller was the RIDER are counterparty records: they are
// the OWNER's history of their own car, and erasing them would delete someone
// else's data. They stay, with rider_id left pointing at a cuid that no longer
// resolves. That is the whole mechanism: requesterIdentitySelect's
// `requester_exists` probe goes false, the scanner leaves RequesterName empty,
// and the wire OMITS `requesterName` — which the client renders as "Former
// rider". No column was added and no row was rewritten; the MYR-264 fallback
// already had exactly this case in it, and MYR-355 only pins it with a test.
type AccountDeletionHandler struct {
	auth   tokenValidator
	deps   AccountDeletionDeps
	logger *slog.Logger
}

// NewAccountDeletionHandler wires the handler. Every field of deps except
// Events and Sessions is required; those two degrade to no-ops (tests, or a
// deployment with no event bus).
func NewAccountDeletionHandler(auth tokenValidator, deps AccountDeletionDeps, logger *slog.Logger) *AccountDeletionHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &AccountDeletionHandler{auth: auth, deps: deps, logger: logger}
}

// ServeHTTP routes DELETE only.
func (h *AccountDeletionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}

	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return
	}

	ctx := r.Context()
	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn("account deletion: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	// No request body is read: the endpoint takes none (rest-api.md §7.6), and
	// accepting one would invite a client to believe it could scope the delete.
	result, failure := h.run(ctx, userID)
	if failure != nil {
		h.logger.Error("account deletion failed",
			slog.String("user_id", userID),
			slog.String("step", failure.step),
			slog.String("error", failure.cause.Error()),
		)
		// 500 and NOTHING else: the sequence is re-runnable, so the honest
		// client behaviour is to retry the same call, and a typed sub-code
		// naming the failed step would leak internal structure for no gain.
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.logger.Info("account_deletion_complete",
		slog.String("user_id", userID),
		slog.Bool("already_gone", result.AlreadyGone),
		slog.Int("vehicles_torn_down", result.Counts.VehicleCount),
		slog.Int("rides_cancelled", result.Counts.RidesCancelled),
		slog.Int("shares_revoked", result.Counts.SharesRevoked),
		slog.Int("push_devices_deleted", result.Counts.PushDevicesDeleted),
		// A COUNT only. The places themselves are P1 (a home address) and
		// never reach a log line, here or anywhere else.
		slog.Int("saved_places_deleted", result.Counts.SavedPlacesDeleted),
		// A COUNT only (MYR-583), and 0 or 1 by the table's key. The row never
		// held the name, so there is nothing P1 here even in principle.
		slog.Int("profile_name_confirmations_deleted", result.Counts.ProfileNameConfirmationsDeleted),
		slog.Int("user_activity_rows_deleted", result.Counts.UserActivityRowsDeleted),
		slog.Int("tesla_token_keepalive_rows_deleted", result.Counts.TeslaTokenKeepaliveRowsDeleted),
		// A COUNT only (MYR-540). Which rides this person had joined is
		// somebody else's ride, and naming them here would put a third party's
		// ride ids in the deleted account's log line.
		slog.Int("ride_memberships_deleted", result.Counts.RideMembershipsDeleted),
		slog.Int("refresh_tokens_revoked", result.Counts.RefreshTokensRevoked),
	)

	// 204 No Content. There is nothing left to describe: the account that
	// would have owned a response body no longer exists, and a body would
	// only tempt a client to render it to a user who is already being signed
	// out. A second call after success is also a 204 (the AlreadyGone arm),
	// which is what makes the client's retry safe.
	w.WriteHeader(http.StatusNoContent)
}

func (h *AccountDeletionHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
