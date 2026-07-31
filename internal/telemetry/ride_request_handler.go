package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// RideRequestHandler serves the rider-facing ride-request REST surface
// (P10, MYR-174): POST /api/ride-requests (create), POST
// /api/ride-requests/{id}/cancel, GET /api/ride-requests/{id} (party-only),
// GET /api/ride-requests (rider's own list). The owner-facing surface
// (incoming feed, accept/decline — MYR-175) is served by the sibling
// RideRequestOwnerHandler.
//
// Authorization model. The create access check is the vehicle owner OR a
// viewer holding an accepted share at the `rides` tier — the top tier, whose
// entire increment over `live_history` is exactly this: "send the car to pick
// them up".
//
// MYR-184 CORRECTED THE PREDICTION THIS COMMENT USED TO MAKE. It previously
// said shared-viewer requests would "land when the access set widens, with no
// change to this handler". That was wrong, and rest-api.md §7.8 repeated the
// same error. The owner-equality check below is a SEPARATE code path from the
// read-side access set: widening GetUserVehicles put shared cars in a viewer's
// list and let them read the snapshot, but this check would still have refused
// every one of their ride requests. Granting `rides` required changing it, and
// this is that change.
//
// The ride's ownerId is still the VEHICLE's owner, never the requester, so a
// rider≠owner request routes to the car's owner for accept/decline exactly as
// an owner's own request does.
type RideRequestHandler struct {
	auth     tokenValidator
	vehicles VehicleSnapshotReader
	store    RideRequestStore
	events   RideEventPublisher
	// shares admits a non-owner rider holding an accepted `rides` share.
	// Nil keeps the endpoint owner-only — the fail-closed default.
	shares VehicleShareReader
	// activities is the Live Activity token registry (MYR-172). Nil leaves the
	// §7.21 endpoints answering 500 — a deployment error, not a runtime state.
	activities LiveActivityRegistry
	// bookedWindowsMax is the widest [from, to) span §7.22 will answer about,
	// INJECTED from store.MaxBookedWindowRange by wiring.go (MYR-385). It is a
	// field rather than a const here because this package must not import
	// internal/store, and a restated literal is a cap that drifts from the one
	// the store is actually built around. Zero — the option not wired — leaves
	// the endpoint answering 500, the same fail-closed reading `activities`
	// gets: a deployment error, not a runtime state.
	bookedWindowsMax time.Duration
	logger           *slog.Logger
}

// RideRequestOption configures optional dependencies on RideRequestHandler.
type RideRequestOption func(*RideRequestHandler)

// WithRideShareReader admits riders who are not the vehicle's owner but hold
// an accepted share at the `rides` tier (MYR-184).
func WithRideShareReader(shares VehicleShareReader) RideRequestOption {
	return func(h *RideRequestHandler) {
		h.shares = shares
	}
}

// NewRideRequestHandler constructs the rider-facing handler. events may be
// nil (WS/dispatch notifications become no-ops) — useful in tests that only
// exercise the HTTP contract.
func NewRideRequestHandler(
	tokens tokenValidator,
	vehicles VehicleSnapshotReader,
	store RideRequestStore,
	publisher RideEventPublisher,
	logger *slog.Logger,
	opts ...RideRequestOption,
) *RideRequestHandler {
	h := &RideRequestHandler{
		auth:     tokens,
		vehicles: vehicles,
		store:    store,
		events:   publisher,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeCancel handles POST /api/ride-requests/{id}/cancel. Rider-only;
// legal only from requested/accepted (→ cancelled). Every other current
// status is a 409 conflict.
func (h *RideRequestHandler) ServeCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	rec, ok := h.loadForParty(ctx, w, r, userID)
	if !ok {
		return
	}
	if userID != rec.RiderID {
		// The owner is a party but cancel is rider-only; non-parties were
		// already 404'd by loadForParty.
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodePermissionDenied, "only the rider may cancel this request")
		return
	}

	if !cancellableFrom(rec.Status) {
		// Friendly fast-path message; the guarded write below is what
		// actually decides under concurrency.
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be cancelled from status "+rec.Status)
		return
	}

	updated, ok := h.mutateStatus(ctx, w, rec, rideCancellableFrom, rideStatusCancelled)
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, toRideRequestWire(updated))
}

// rideCancellableFrom is the allowed-from set for a rider cancel; must stay
// in lockstep with cancellableFrom and the rest-api.md §7.8 matrix.
var rideCancellableFrom = []string{rideStatusRequested, rideStatusAccepted}

// The guarded-transition helpers (mutateStatus and its dormancy-guarded
// sibling) live in ride_request_status_mutation.go.

// cancellableFrom reports whether a rider cancel is legal from the given
// status. Legal only from requested/accepted; enroute/arrived (ride in
// progress) and the terminal states (declined/completed/cancelled) are not.
func cancellableFrom(status string) bool {
	return status == rideStatusRequested || status == rideStatusAccepted
}

// loadForParty fetches the ride by {id} and enforces party membership: a
// caller who is neither rider nor owner gets a 404 (no existence leak). On
// success returns the record; on any failure writes the response and returns
// ok=false.
func (h *RideRequestHandler) loadForParty(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) (RideRequestData, bool) {
	id := r.PathValue("id")
	if id == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing ride request id")
		return RideRequestData{}, false
	}

	rec, err := h.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "ride request not found")
			return RideRequestData{}, false
		}
		h.logger.Error("ride-request: lookup failed",
			slog.String("ride_request_id", id),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return RideRequestData{}, false
	}

	if userID != rec.RiderID && userID != rec.OwnerID {
		// Non-party: return 404 rather than 403 so the server never
		// confirms the existence of a ride the caller has no relation to.
		h.logger.Warn("ride-request: non-party access",
			slog.String("ride_request_id", id),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "ride request not found")
		return RideRequestData{}, false
	}

	return rec, true
}

// authUser extracts + validates the bearer token, returning the userID.
func (h *RideRequestHandler) authUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return "", false
	}
	userID, err := h.auth.ValidateToken(r.Context(), token)
	if err != nil {
		h.logger.Warn("ride-request: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return "", false
	}
	return userID, true
}

// publish builds an Event envelope for the payload and publishes it,
// swallowing (logging) errors — the DB mutation already committed, so a
// dropped notification must not fail the HTTP request.
func (h *RideRequestHandler) publish(ctx context.Context, payload events.EventPayload) {
	if h.events == nil {
		return
	}
	if err := h.events.Publish(ctx, events.NewEvent(payload)); err != nil {
		h.logger.Warn("ride-request: publish event failed",
			slog.String("topic", string(payload.EventTopic())),
			slog.String("error", err.Error()),
		)
	}
}

// decodeCreateBody strictly decodes the request body (unknown keys are a 400,
// matching the schema's additionalProperties:false).
func (h *RideRequestHandler) decodeCreateBody(w http.ResponseWriter, r *http.Request) (rideRequestCreateBody, bool) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body rideRequestCreateBody
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return rideRequestCreateBody{}, false
	}
	return body, true
}

// writeJSON marshals v as JSON with the given status code.
func (h *RideRequestHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("ride-request: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1).
func (h *RideRequestHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}

// writeErrorSub writes the same envelope with a typed sub-code, for the paths
// where the primary code does not tell the client what to do on its own.
func (h *RideRequestHandler) writeErrorSub(w http.ResponseWriter, status int, code wserrors.ErrorCode, sub wserrors.SubCode, msg string) {
	wserrors.WriteErrorEnvelopeSub(w, h.logger, status, code, sub, msg)
}

// writeRideActive writes the 409 `ride_active` response (MYR-230): the
// standard error envelope plus the rider's existing OPEN instant ride under
// `activeRideRequest`, so the client adopts it into the pending/tracking UI.
// The message carries no P1 value; the adopted ride's coordinates go only to
// its own rider (a party, mirroring GET) and are never logged here.
func (h *RideRequestHandler) writeRideActive(w http.ResponseWriter, existing RideRequestData) {
	h.writeJSON(w, http.StatusConflict, rideActiveErrorResponse{
		Error: wserrors.ErrorEnvelopeBody{
			Code:    wserrors.ErrCodeRideActive,
			Message: "you already have an active ride request",
		},
		ActiveRideRequest: toRideRequestWire(existing),
	})
}
