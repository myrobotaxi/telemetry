// POST /api/ride-requests/join — the JOINER-side redemption of a group ride's
// shared link (MYR-540, rest-api.md §7.24, contracts $defs.RideRequestJoinRequest).
//
// It mirrors POST /api/invites/redeem (§7.5.5) rule for rule, because it is the
// same act with a different subject: a code, held by whoever received the link,
// exchanged for access. Same body-not-path placement, same normalization, same
// per-user rate limit, same deliberate indistinguishability of every dead code.
//
// WHAT IS DIFFERENT IS THE RESPONSE. The sharing redeem needed its own response
// shape because ShareInvite is owner-facing and could never be handed to the
// redeemer; the ride a joiner joins IS the object they are now a party to, so
// the 200 body is the full RideRequest with the caller already in `members` —
// and the joiner goes straight to the rider tracking surface with no second
// round trip.
//
// AUTHENTICATED, and that is a product decision before it is an implementation
// one (client, 2026-08-13): app plus sign-in are required, the joining account
// is the JWT subject and is never client-supplied, and live car positions never
// render on the open web. The web landing shell at /ride/{code} verifies the
// link's SIGNATURE statically against a compiled-in public key and then hands
// the bare code here; the signature proves the link came from this server, and
// this endpoint is the only thing that decides whether it still means anything.

package telemetry

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// ErrRideJoinSelfParty is returned by RideRequestStore.JoinByCode when the
// caller is already a party to the ride in another role — its requester, or the
// vehicle's owner. The handler maps it to 409 `conflict`. The cmd adapter
// translates store.ErrRideJoinSelfParty into it so the handler layer stays
// decoupled from internal/store.
var ErrRideJoinSelfParty = errors.New("already a party to this ride")

// rideJoinRequest is the POST body (contracts $defs.RideRequestJoinRequest):
// `{"code":"RBO246"}` and nothing else.
type rideJoinRequest struct {
	Code string `json:"code"`
}

// ServeJoin handles POST /api/ride-requests/join.
func (h *RideRequestHandler) ServeJoin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	// Rate-limit BEFORE the body is examined, so a malformed-body flood cannot
	// be used to probe timing, and count the attempt whatever its outcome. The
	// budget is this endpoint's OWN — a separate limiter instance from the
	// invite redeem's, with the same numbers — because the two code spaces are
	// separate and spending one endpoint's allowance should not close the other.
	if !h.joinLimiter.allow(userID) {
		h.logger.Warn("ride join: rate limited", slog.String("user_id", userID))
		h.writeError(w, http.StatusTooManyRequests, wserrors.ErrCodeRateLimited,
			"too many join attempts — try again in a minute")
		return
	}

	code, ok := h.decodeJoinCode(w, r)
	if !ok {
		return
	}

	rec, created, err := h.store.JoinByCode(ctx, code, userID)
	if err != nil {
		h.writeJoinError(w, userID, err)
		return
	}

	if created {
		// Only the redemption that actually inserted a row announces itself; an
		// idempotent re-join is not news. Published AFTER the commit and
		// swallowed on failure, like every other ride publish — the membership
		// is live either way.
		h.publish(ctx, events.RideMemberJoinedEvent{
			RideRequestID: rec.ID,
			VehicleID:     rec.VehicleID,
			RiderID:       rec.RiderID,
			OwnerID:       rec.OwnerID,
			MemberID:      userID,
			JoinedAt:      rec.UpdatedAt,
		})
		// The member's access set just widened to include this ride's VEHICLE
		// (auth's third UNION leg). Bust their cached set so the car resolves on
		// their very next request rather than after the TTL — the redeem path's
		// own rule, and the same failure it prevents: a "you're in!" screen
		// followed by a tracking sheet that cannot see the car.
		if h.access != nil {
			h.access.InvalidateVehicles(userID)
		}
		h.logger.Info("ride join: member joined",
			slog.String("ride_request_id", rec.ID),
			slog.String("user_id", userID),
		)
	}

	h.writeJSON(w, http.StatusOK, h.rideWire(rec))
}

// decodeJoinCode reads and normalizes the submitted code.
//
// Normalization mirrors the client's entry field and the invite redeem's
// (upper-case, strip everything outside [A-Z0-9]) so a code that survived a
// copy-paste with a stray space still redeems. A code that is STILL malformed
// after that is 400 — never 404 — because the 404 body is reserved for "this
// code does not get you onto a ride", and blurring the two would make the
// deliberate unknown/expired/terminal indistinguishability harder to reason
// about, not easier.
//
// The code is NEVER logged and never echoed into the error body: it is a live
// bearer credential for a ride's live location, and an error message repeating
// it is a confirmation oracle.
func (h *RideRequestHandler) decodeJoinCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body rideJoinRequest
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return "", false
	}

	code := normalizeShareCode(body.Code)
	if !validShareCodeFormat(code) {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"code must be 6 characters, A-Z and 0-9")
		return "", false
	}
	return code, true
}

// writeJoinError maps a redemption failure.
//
// THE THREE DEAD-CODE CASES — unknown, expired, and a ride that has reached a
// terminal status — ALL answer 404 WITH THE SAME BODY. That is the contract and
// it is the point: an enumerating caller must not learn that a guessed code
// exists but belongs to a ride that has already finished, which is a hit worth
// following up. They arrive here as one sentinel because the store's own query
// collapses them (see store.ErrRideJoinCodeNotFound), so this function could not
// tell them apart even if a later edit wanted to.
//
// The 409 is deliberately NOT one of them: a caller who is already the requester
// or the owner can see the ride in their own app, so there is nothing to
// conceal, and answering "not found" to them would read as a bug.
func (h *RideRequestHandler) writeJoinError(w http.ResponseWriter, userID string, err error) {
	switch {
	case errors.Is(err, sdk.ErrNotFound):
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "ride not found")
	case errors.Is(err, ErrRideJoinSelfParty):
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"you are already on this ride")
	default:
		// The code is P1 and is deliberately absent from this log line.
		h.logger.Error("ride join failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
	}
}

// WithRideAccessInvalidator wires the access-set cache bust a successful join
// performs (MYR-540). Nil leaves the new member's vehicle appearing on the next
// cache expiry instead of immediately.
func WithRideAccessInvalidator(invalidator AccessCacheInvalidator) RideRequestOption {
	return func(h *RideRequestHandler) {
		h.access = invalidator
	}
}
