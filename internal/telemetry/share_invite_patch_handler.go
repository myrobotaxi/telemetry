package telemetry

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// PATCH /api/invites/{inviteId} — the owner editing ONE accepted grant's
// capabilities in place (MYR-369, rest-api.md §7.5.7).
//
// This endpoint is what replaced the pre-MYR-369 rule that a grant's access was
// fixed for its life and changing it meant revoke-plus-reinvite. It is ACCESS
// CONTROL and is written for adversarial reading:
//
//   - OWNER-ONLY, enforced IN THE SQL (`owner_user_id = $n` on the UPDATE
//     itself). Like ServeRevoke and ServeResend it does NOT pre-read the row to
//     check ownership: a read-then-write would add a window and a second thing
//     to disagree, and would buy nothing the predicate does not already give.
//   - 404-INDISTINGUISHABILITY IS PRESERVED. A missing invite, another owner's
//     invite, and a revoked tombstone all answer 404 with one body, so this
//     endpoint cannot be used to probe for other people's invite ids — exactly
//     the property DELETE and resend already hold.
//   - ATOMIC. One conditional UPDATE with RETURNING; the response echoes the row
//     the DATABASE now holds rather than a Go-side reconstruction of the request,
//     so a concurrent edit is reported honestly instead of being papered over.
//   - PARTIAL means partial. An absent key leaves that capability untouched; it
//     is NOT the same as sending `false`. Pointer fields on the request struct
//     are what carry the distinction, and an entirely empty body is 400 rather
//     than a successful no-op.

// ServePatch handles PATCH /api/invites/{inviteId}.
//
// 200 with the updated ShareInvite. Errors:
//   - 400 invalid_request — malformed body, or a body that changes nothing.
//   - 404 not_found — missing / foreign / revoked, indistinguishably.
//   - 409 conflict — the invite is still PENDING. A pending row has no grant to
//     edit; its access is decided at redemption from the invite's preset, so the
//     owner cancels and re-sends instead. Told plainly because the caller
//     demonstrably owns the row, so there is nothing left to hide.
func (h *ShareInviteHandler) ServePatch(w http.ResponseWriter, r *http.Request) {
	inviteID, userID, ok := h.authInvite(w, r, "share invite patch")
	if !ok {
		return
	}

	patch, ok := h.decodePatch(w, r)
	if !ok {
		return
	}

	row, granteeID, err := h.invites.PatchInvite(r.Context(), inviteID, userID, patch)
	if err != nil {
		h.writePatchError(w, inviteID, err)
		return
	}

	// BUST THE GRANTEE'S CACHE, NOT THE OWNER'S (the MYR-184 bust-on-mutation
	// pattern). The cached access set belongs to the person whose access
	// changed, and for a suspension the bust is a security property exactly
	// as it is for a revoke: the cache is what the WebSocket handshake and
	// every per-vehicle handler consult, so a stale entry IS a live grant for
	// up to the TTL.
	//
	// UNCONDITIONAL — every successful patch busts, not only the suspending
	// one. A bust conditional on which field moved is a rule that can be got
	// wrong by the next person to add a field; an unconditional one cannot,
	// and the cost is one dropped cache entry on an already-rare owner action.
	if h.access != nil && granteeID != "" {
		h.access.InvalidateVehicles(granteeID)
	}

	// The invite id and what changed — never the label (P1), and there is no
	// code on an accepted row to leak.
	h.logger.Info("share invite patched",
		slog.String("invite_id", inviteID),
		slog.Bool("allow_rides", row.Grant.AllowRides),
		slog.Bool("suspended", row.Grant.Suspended),
	)
	h.writeJSON(w, http.StatusOK,
		toShareInviteMasked(&row, auth.RoleOwner, h.linkCtx(r.Context(), userID)))
}

// decodePatch reads the body and rejects one that asks for nothing.
//
// An EMPTY PATCH IS 400, not a 200 echo. The store refuses it too, but the
// refusal belongs here as well: succeeding on a request that changes nothing
// lets a client bug — a field name typo, a serializer that dropped a key —
// present as an applied edit, and on an access-control surface "I turned it off
// and it said OK" is the worst possible failure mode.
//
// Unknown keys are IGNORED rather than rejected, matching every other body on
// this surface: the schema marks the object additionalProperties:false, but
// enforcing that here would break a client that sends a field a later version
// adds, and the fields this server acts on are exactly the two it reads.
func (h *ShareInviteHandler) decodePatch(w http.ResponseWriter, r *http.Request) (ShareInvitePatch, bool) {
	var body patchShareInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return ShareInvitePatch{}, false
	}

	// A direct conversion, not a field-by-field copy: the two types are
	// structurally identical by construction, and a copy is where one of them
	// would eventually gain a field the other silently dropped.
	patch := ShareInvitePatch(body)
	if !patch.HasChange() {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
			"provide at least one of allowRides or suspended")
		return ShareInvitePatch{}, false
	}
	return patch, true
}

// writePatchError maps a patch failure.
//
// It does NOT reuse writeInviteError, because the two surfaces disagree on one
// status and the disagreement is deliberate: a resend against an ACCEPTED row is
// 409 (ErrShareNotPending), a patch against a PENDING row is 409 for the
// opposite reason (ErrShareNotAccepted), and folding them into one switch would
// invite a future edit that made one of them answer the other's message.
func (h *ShareInviteHandler) writePatchError(w http.ResponseWriter, inviteID string, err error) {
	switch {
	case errors.Is(err, sdk.ErrNotFound):
		// Missing, foreign, and revoked — one body for all three.
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "invite not found")
	case errors.Is(err, ErrShareNotAccepted):
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"this invite has not been accepted yet — cancel it and send a new one to change its access")
	default:
		h.logger.Error("share invite patch failed",
			slog.String("invite_id", inviteID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
	}
}
