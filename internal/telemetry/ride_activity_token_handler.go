package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Live Activity token registration (MYR-172, rest-api.md §7.21).
//
// The iOS app starts a Live Activity locally when a ride is accepted and is
// handed a per-Activity push token by ActivityKit. These two endpoints are how
// that token reaches the server, and how the app says the Activity is over.
//
// RIDER-ONLY, deliberately. The owner is a party to the ride and can read it,
// but v1 starts no owner Activity, and an endpoint that accepted an owner's
// token would quietly create rows the sender would then push RIDER content to —
// including the destination label, which the owner has no business seeing on
// their lock screen. The owner variant is an explicit MYR-172 follow-up and
// will arrive with its own content-state.
//
// The token ROTATES. ActivityKit hands the app a replacement mid-ride and
// expects the server to switch to it, so registration is an UPSERT on
// (ride, party) rather than an insert, and a rotation is an ordinary
// re-registration. A client that re-registers after ending clears the
// tombstone, because it is telling us it has a live Activity again.

// maxActivityTokenLen bounds an accepted token. An ActivityKit push token is
// 64 hex characters today, the same shape as a device token, but the bound is
// deliberately generous for the same reason as maxDeviceTokenLen in
// internal/push: rejecting a valid future token would silently break Live
// Activities for everyone on that iOS release, which is far worse than storing
// a slightly odd string.
const maxActivityTokenLen = 256

// LiveActivityRegistry is the persistence seam for these two endpoints.
// Satisfied by *store.LiveActivityRepo directly; no adapter needed.
type LiveActivityRegistry interface {
	RegisterActivity(ctx context.Context, rideRequestID, userID, token string, sandbox bool) error
	EndActivity(ctx context.Context, rideRequestID, userID string) (bool, error)
}

// WithLiveActivityRegistry wires the Live Activity token endpoints. Nil leaves
// them answering 500 — the fail-closed default, and a deployment error rather
// than a runtime state.
func WithLiveActivityRegistry(registry LiveActivityRegistry) RideRequestOption {
	return func(h *RideRequestHandler) {
		h.activities = registry
	}
}

// activityTokenRequest is the POST body: `{"activityToken":"…","sandbox":true}`.
type activityTokenRequest struct {
	ActivityToken string `json:"activityToken"`
	Sandbox       bool   `json:"sandbox"`
}

// activityTokenResponse confirms a registration.
//
// It deliberately does NOT echo the token: the token is P1
// (data-classification.md §1.18) and echoing it would put it in every client
// log and proxy trace for no benefit — the caller already knows what it sent.
// Same rule, same reason, as the §7.17 device-registration response.
type activityTokenResponse struct {
	Registered bool `json:"registered"`
	Sandbox    bool `json:"sandbox"`
}

// activityEndedResponse reports whether a live registration was actually
// closed. `false` covers both "already ended" and "never registered",
// deliberately indistinguishable — though on this endpoint the caller has
// already been proven to be the ride's rider, so there is nothing to probe.
type activityEndedResponse struct {
	Ended bool `json:"ended"`
}

// ServeRegisterActivityToken handles
// POST /api/ride-requests/{id}/activity-token.
func (h *RideRequestHandler) ServeRegisterActivityToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rec, ok := h.authRiderForActivity(w, r)
	if !ok {
		return
	}

	body, ok := h.decodeActivityTokenBody(w, r)
	if !ok {
		return
	}

	token := strings.TrimSpace(body.ActivityToken)
	if token == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "activityToken is required")
		return
	}
	if len(token) > maxActivityTokenLen {
		// The token is P1: report the violation, never the value.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "activityToken is too long")
		return
	}

	// A ride that has already ended will never be pushed to again — the
	// terminal `event: "end"` has fired and the rows are tombstoned — so
	// accepting a registration here would store a row nothing will ever update
	// and only the 24h sweep would remove. The 409 tells the client to end its
	// Activity locally now, which is exactly what its final-state fallback is
	// for.
	if isTerminalRideStatus(rec.Status) {
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict,
			"ride request is already "+rec.Status)
		return
	}

	if err := h.activities.RegisterActivity(ctx, rec.ID, rec.RiderID, token, body.Sandbox); err != nil {
		h.logger.Error("ride-request: activity token registration failed",
			slog.String("ride_request_id", rec.ID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, activityTokenResponse{Registered: true, Sandbox: body.Sandbox})
}

// ServeEndActivityToken handles DELETE /api/ride-requests/{id}/activity-token.
//
// Called when the Activity ends on the phone — the rider dismissed it, or the
// app ended it from its own final-state fallback. Idempotent: ending an
// already-ended Activity is a 200 reporting `false`, not an error, because the
// client's end and a terminal-state send race by design and both are correct.
func (h *RideRequestHandler) ServeEndActivityToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rec, ok := h.authRiderForActivity(w, r)
	if !ok {
		return
	}

	ended, err := h.activities.EndActivity(ctx, rec.ID, rec.RiderID)
	if err != nil {
		h.logger.Error("ride-request: activity token end failed",
			slog.String("ride_request_id", rec.ID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, activityEndedResponse{Ended: ended})
}

// authRiderForActivity runs the shared front half of both endpoints:
// authenticate, resolve the ride, and require the caller to be its RIDER.
//
// The 404-vs-403 split is the house rule, unchanged (rest-api.md §5.2):
// loadForParty answers 404 for a stranger — so the endpoint never confirms that
// a ride id exists to somebody with no relation to it — and only a genuine
// party who is the OWNER rather than the rider reaches the 403 below.
func (h *RideRequestHandler) authRiderForActivity(w http.ResponseWriter, r *http.Request) (RideRequestData, bool) {
	ctx := r.Context()

	userID, ok := h.authUser(w, r)
	if !ok {
		return RideRequestData{}, false
	}

	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return RideRequestData{}, false
	}
	if userID != rec.RiderID {
		// The owner is a party but Live Activities are rider-only in v1;
		// non-parties were already 404'd by loadForParty.
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied,
			"only the rider may register a Live Activity for this ride")
		return RideRequestData{}, false
	}

	if h.activities == nil {
		h.logger.Error("ride-request: live activity registry not wired")
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return RideRequestData{}, false
	}

	return rec, true
}

// decodeActivityTokenBody strictly decodes the POST body (unknown keys are a
// 400, matching the schema's additionalProperties:false).
func (h *RideRequestHandler) decodeActivityTokenBody(w http.ResponseWriter, r *http.Request) (activityTokenRequest, bool) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body activityTokenRequest
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return activityTokenRequest{}, false
	}
	return body, true
}

// isTerminalRideStatus reports whether a ride has finished for good.
//
// Kept in lockstep with push.terminalStatuses, which decides the same question
// on the send side. The two cannot share a constant without internal/telemetry
// depending on internal/push, so they are pinned together by a test instead.
func isTerminalRideStatus(status string) bool {
	switch status {
	case rideStatusCompleted, rideStatusDeclined, rideStatusCancelled:
		return true
	default:
		return false
	}
}
