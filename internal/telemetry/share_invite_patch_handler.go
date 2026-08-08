package telemetry

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

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

	// AND TEAR DOWN A LIVE SOCKET, but only when the patch left the grant
	// SUSPENDED (MYR-373). The bust above is unconditional for the reason
	// given there; this is not, and the asymmetry is deliberate:
	//
	//   - Suspension is the only flag on this endpoint that removes the
	//     grant from the viewer-merge access set, which is the single thing
	//     the WS handshake resolves through. Losing it is what has to stop a
	//     live stream.
	//   - allowRides governs the §7.8 ride surface and has NO WebSocket
	//     effect whatsoever. Closing a socket over it would drop a viewer's
	//     live map for a change that does not touch the live map.
	//
	// Read off the ROW the database returned rather than off the request, so
	// an already-suspended grant patched for some other reason still closes
	// (harmless, and the alternative is trusting the request to describe the
	// resulting state). A patch that lifts a suspension leaves Suspended
	// false and closes nothing — restoring access has no socket to end.
	if row.Grant.Suspended {
		h.endLiveAccess(granteeID, row.VehicleID, "suspended")
	}

	// The invite id and what changed — never the label (P1), and there is no
	// code on an accepted row to leak.
	//
	// MYR-451 WIDENED THIS DELIBERATELY, and the reason is worth keeping. The
	// line used to carry the invite id and the two resulting flags, which is
	// enough to know a patch happened and useless for the question an incident
	// actually asks: WHICH PERSON lost WHICH capability on WHICH CAR, and did
	// that precede or follow the ride they took. An invite id joins to none of
	// that without a database the on-call may not have, and by then the row has
	// moved on — a revoked grant answers 404 and a re-invite reuses nothing.
	//
	// So the line now names the grantee and the vehicle, which are exactly the
	// two keys denyVehicleAccess and the create-path decision log key on. The
	// three lines join on (user_id, vehicle_id) and reconstruct the timeline
	// from logs alone. `requested_*` records what the owner ASKED FOR (nil for
	// an absent field, which is not the same as false) beside what the row now
	// HOLDS, so a partial patch reads unambiguously and a request that changed
	// nothing is visible as such rather than looking like a fresh decision.
	h.logger.Info("share invite patched",
		slog.String("invite_id", inviteID),
		slog.String("owner_user_id", userID),
		slog.String("user_id", granteeID),
		slog.String("vehicle_id", row.VehicleID),
		requestedFlag("requested_allow_rides", patch.AllowRides),
		requestedFlag("requested_suspended", patch.Suspended),
		slog.Bool("allow_rides", row.Grant.AllowRides),
		slog.Bool("suspended", row.Grant.Suspended),
	)
	// NO LINK CONTEXT, AND DELIBERATELY NO DATABASE LOOKUP TO BUILD ONE.
	// inviteLinkCtx exists to mint `shareUrl`, and toShareInviteWire consumes it
	// inside the PENDING branch only — the branch that also emits `code`, since
	// a share link is a wrapper around the credential. A patch cannot return a
	// pending row: queryPatchShare carries `status = 'accepted'` in the UPDATE
	// itself, so a pending id updates zero rows and leaves here as the 409
	// above. The row this serializes is therefore always accepted, always
	// codeless, and always link-less.
	//
	// Passing h.linkCtx(...) here cost an OwnerFirstName query on EVERY
	// successful patch whose only possible destination was a branch that cannot
	// execute — and worse, when that query failed it logged "owner name lookup
	// failed, link omits from", warning an operator about a degraded link that
	// was never going to be minted. A wasted round trip is cheap; a warning that
	// sends somebody looking for a broken share link is not.
	h.writeJSON(w, http.StatusOK,
		toShareInviteMasked(&row, auth.RoleOwner, inviteLinkCtx{}))
}

// requestedFlag renders one OPTIONAL patch field for the log (MYR-451).
//
// It exists because `slog.Any` over a *bool is a trap: the JSON handler
// dereferences it and prints `true`, the text handler prints the POINTER
// (`0xc000…`), and the two disagreeing is discovered during an incident, on the
// one line that was supposed to settle the incident. Rendering the distinction
// ourselves makes the attribute identical under every handler.
//
// An absent field logs "unchanged" rather than being omitted or logged as
// false. Omission would make a partial patch indistinguishable from one that
// never mentioned the field, and false would assert a decision the owner did
// not make — the very conflation §7.5.7 spends a paragraph forbidding on the
// wire, which a log line has no business reintroducing.
//
// ALWAYS A STRING, INCLUDING FOR THE PRESENT CASE, and that is the whole reason
// this returns what it returns. Emitting `slog.Bool` when present and
// `slog.String` when absent would put two JSON TYPES under one key across
// consecutive lines; a strict-mapping sink (Elasticsearch dynamic mapping,
// BigQuery, Loki structured metadata) rejects the second shape and DROPS THE
// DOCUMENT — silently discarding exactly the line written to settle an
// incident. One key, one type, three self-describing values.
func requestedFlag(name string, v *bool) slog.Attr {
	if v == nil {
		return slog.String(name, "unchanged")
	}
	return slog.String(name, strconv.FormatBool(*v))
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
