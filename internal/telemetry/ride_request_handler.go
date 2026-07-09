package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

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
// Authorization model (v1): the create access check reuses the existing
// vehicle-access pattern (VehicleSnapshotReader.GetByID + owner match, same
// as /snapshot and /drives), so today only a caller who owns the vehicle may
// request a ride on it and the ride's ownerId is that same owner. Broader
// shared-viewer requests (rider != owner) are DEFERRED to the app-side
// sharing tiers: they land when the server's vehicle-access set gains the
// viewer-merge pathway (GetUserVehicles, PLANNED MYR-91) — no change to this
// handler is needed, only a wider access set.
type RideRequestHandler struct {
	auth     tokenValidator
	vehicles VehicleSnapshotReader
	store    RideRequestStore
	events   RideEventPublisher
	logger   *slog.Logger
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
) *RideRequestHandler {
	return &RideRequestHandler{
		auth:     tokens,
		vehicles: vehicles,
		store:    store,
		events:   publisher,
		logger:   logger,
	}
}

// ServeCreate handles POST /api/ride-requests.
func (h *RideRequestHandler) ServeCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}

	body, ok := h.decodeCreateBody(w, r)
	if !ok {
		return
	}

	in, reason := validateCreateBody(body)
	if reason != "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, reason)
		return
	}

	// Derive ownerId from the vehicle + enforce vehicle access (v1: owner
	// match; see the type doc for the deferred shared-viewer expansion).
	row, err := h.vehicles.GetByID(ctx, in.VehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return
		}
		h.logger.Error("ride-request create: vehicle lookup failed",
			slog.String("vehicle_id", in.VehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}
	if row.UserID != userID {
		h.logger.Warn("ride-request create: vehicle access denied",
			slog.String("vehicle_id", in.VehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not have access to this vehicle")
		return
	}
	in.RiderID = userID
	in.OwnerID = row.UserID

	created, err := h.store.Create(ctx, in)
	if err != nil {
		h.logger.Error("ride-request create: store failed",
			slog.String("vehicle_id", in.VehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.publish(ctx, events.RideRequestCreatedEvent{
		RideRequestID: created.ID,
		VehicleID:     created.VehicleID,
		RiderID:       created.RiderID,
		OwnerID:       created.OwnerID,
		Status:        created.Status,
		ScheduledFor:  created.ScheduledFor,
		CreatedAt:     created.CreatedAt,
	})

	h.writeJSON(w, http.StatusCreated, toRideRequestWire(created))
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
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot be cancelled from status "+rec.Status)
		return
	}

	h.applyTransition(ctx, w, rec, rideStatusCancelled)
}

// applyTransition persists a status change, publishes the summary
// ride_status_changed event, and writes the updated RideRequest. Shared by
// cancel (MYR-174) and accept/decline (MYR-175).
func (h *RideRequestHandler) applyTransition(ctx context.Context, w http.ResponseWriter, rec RideRequestData, status string) {
	updated, err := h.store.UpdateStatus(ctx, rec.ID, status)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			// Raced with a delete between GetByID and UpdateStatus.
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "ride request not found")
			return
		}
		h.logger.Error("ride-request transition: store failed",
			slog.String("ride_request_id", rec.ID),
			slog.String("status", status),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.publish(ctx, events.RideStatusChangedEvent{
		RideRequestID:    updated.ID,
		VehicleID:        updated.VehicleID,
		RiderID:          updated.RiderID,
		OwnerID:          updated.OwnerID,
		Status:           updated.Status,
		RescheduleStatus: updated.RescheduleStatus,
		UpdatedAt:        updated.UpdatedAt,
	})

	h.writeJSON(w, http.StatusOK, toRideRequestWire(updated))
}

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
