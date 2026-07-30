package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/auth"
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
	logger *slog.Logger
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

	// Derive ownerId from the vehicle + enforce vehicle access: the owner,
	// or a viewer at the `rides` tier (see the type doc).
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
	// `rides` is the TOP tier and its whole increment is this endpoint, so a
	// viewer at `live` or `live_history` is refused here even though they can
	// see the car and (at live_history) its trip history.
	if _, accessErr := vehicleAccessFor(ctx, h.shares, userID, in.VehicleID, row.UserID, auth.PermissionRides); accessErr != nil {
		if errors.Is(accessErr, errNoVehicleAccess) {
			denyVehicleAccess(w, h.logger, "ride-request create", in.VehicleID, userID)
			return
		}
		h.logger.Error("ride-request create: access resolution failed",
			slog.String("vehicle_id", in.VehicleID),
			slog.String("error", accessErr.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}
	// MYR-342: the vehicle's owner may have PAUSED ride requests for this car.
	// Placed immediately after the access check and BEFORE anything is derived
	// or written, so a caller with no access to the vehicle still gets the
	// access denial rather than learning its pause state (ride_share_gate.go).
	//
	// Applies to INSTANT AND SCHEDULED creates alike — a deliberate deviation
	// from the MYR-313 scheduled exemption below it. A service visit ends on its
	// own, so a reservation booked across one is likely servable; an owner's
	// pause is open-ended, so a reservation booked against a paused car is not.
	// The flag is read off `row`, the very lookup that resolved the owner, so
	// access and availability come from one statement and cannot disagree.
	if rejectIfRideSharePaused(w, h.logger, "ride-request create", row, userID) {
		return
	}

	in.RiderID = userID
	// The ride's owner is the VEHICLE's owner, which for a shared rider is
	// somebody other than the requester — that asymmetry is the point of the
	// `rides` tier, and it is what routes the accept/decline to the right
	// person.
	in.OwnerID = row.UserID

	// MYR-316: a scheduled ride may not be booked for a time before the car is
	// expected back from service. Instant rides are unaffected (already gated
	// by MYR-277), and a vehicle with no estimate imposes no bound.
	if h.rejectIfBeforeServiceWindow(w, in.ScheduledFor, row) {
		return
	}

	// One active ride per rider (MYR-230): an INSTANT request is refused
	// while the rider already has an open instant ride. The fast-path
	// pre-check runs here; the partial unique index (migration 0004) is the
	// authoritative race backstop, applied to the Create error below.
	if h.rejectIfRideActive(ctx, w, in) {
		return
	}

	created, err := h.store.Create(ctx, in)
	if err != nil {
		h.handleCreateError(ctx, w, in, err)
		return
	}

	h.publish(ctx, events.RideRequestCreatedEvent{
		RideRequestID: created.ID,
		VehicleID:     created.VehicleID,
		RiderID:       created.RiderID,
		OwnerID:       created.OwnerID,
		Status:        created.Status,
		RequesterName: created.RequesterName,
		ScheduledFor:  created.ScheduledFor,
		CreatedAt:     created.CreatedAt,
	})

	h.writeJSON(w, http.StatusCreated, toRideRequestWire(created))
}

// rejectIfRideActive is the one-active-instant-ride fast path (MYR-230). For
// an INSTANT request it 409s `ride_active` (carrying the existing ride to
// adopt) when the rider already has an open instant ride, or 500s on a lookup
// failure — returning true in either case so ServeCreate stops. Scheduled
// requests are EXEMPT (a future reservation is not an active ride) and always
// pass through (returns false). The partial unique index stays the
// authoritative race guard, enforced by handleCreateError.
func (h *RideRequestHandler) rejectIfRideActive(ctx context.Context, w http.ResponseWriter, in RideRequestCreateInput) bool {
	if in.ScheduledFor != nil {
		return false
	}
	switch existing, err := h.store.GetActiveInstantByRider(ctx, in.RiderID); {
	case err == nil:
		h.writeRideActive(w, existing)
		return true
	case errors.Is(err, sdk.ErrNotFound):
		return false // no open instant ride — proceed to create
	default:
		h.logger.Error("ride-request create: active-ride pre-check failed",
			slog.String("user_id", in.RiderID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return true
	}
}

// handleCreateError maps a failed store.Create onto a response. ErrRideActive
// (the unique-index race backstop refusing a concurrent second instant
// create) re-reads the winning ride so the client adopts it, exactly as the
// pre-check would; every other error is a 500.
func (h *RideRequestHandler) handleCreateError(ctx context.Context, w http.ResponseWriter, in RideRequestCreateInput, err error) {
	if errors.Is(err, ErrRideActive) {
		if existing, gErr := h.store.GetActiveInstantByRider(ctx, in.RiderID); gErr == nil {
			h.writeRideActive(w, existing)
			return
		}
		// Extreme TOCTOU: the conflicting ride reached a terminal state
		// between the rejected insert and this re-read. Surface the typed 409
		// without a body rather than a generic failure; the SDK re-syncs its
		// own list and may retry the create.
		h.logger.Warn("ride-request create: active guard fired but open ride vanished",
			slog.String("user_id", in.RiderID),
		)
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeRideActive, "you already have an active ride request")
		return
	}
	h.logger.Error("ride-request create: store failed",
		slog.String("vehicle_id", in.VehicleID),
		slog.String("error", err.Error()),
	)
	h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
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

// mutateStatus persists a status change ATOMICALLY via the store's guarded
// UpdateStatusFrom — a single UPDATE with `WHERE status = ANY(from)` — so a
// check-then-write race (rider-cancel vs owner-decline, owner double-accept)
// resolves in the database: exactly one caller wins; the loser gets a 409
// conflict here even if its pre-check read passed. Only the winning call
// publishes the ride_status_changed summary event (and, for accept, the
// ride.accepted dispatch seam at the call site) — exactly-once by
// construction. Does NOT write the success response; the caller does. On
// failure writes the error envelope and returns ok=false. Shared by cancel
// (MYR-174) and accept/decline (MYR-175).
func (h *RideRequestHandler) mutateStatus(ctx context.Context, w http.ResponseWriter, rec RideRequestData, from []string, to string) (RideRequestData, bool) {
	updated, err := h.store.UpdateStatusFrom(ctx, rec.ID, from, to)
	if err != nil {
		switch {
		case errors.Is(err, ErrVehicleRideActive):
			// Per-vehicle one-active-ride guard (MYR-266): the transition is
			// legal for THIS ride, but the target vehicle is already committed
			// to another active ride — a capability gate on the vehicle, the
			// same 409 vehicle_unavailable class as the MYR-277 in-service /
			// offline gate (the car cannot serve a second ride at once), NOT the
			// illegal-transition `conflict`.
			h.writeError(w, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable, "Vehicle is already on another ride")
		case errors.Is(err, ErrRideStatusConflict):
			// Lost the transition race (or the pre-check read was stale):
			// the row's current status left the allowed-from set between
			// our read and the guarded write.
			h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "ride request cannot transition to "+to+" from its current status")
		case errors.Is(err, sdk.ErrNotFound):
			// Raced with a delete between GetByID and the guarded write.
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "ride request not found")
		default:
			h.logger.Error("ride-request transition: store failed",
				slog.String("ride_request_id", rec.ID),
				slog.String("status", to),
				slog.String("error", err.Error()),
			)
			h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		}
		return RideRequestData{}, false
	}

	h.publish(ctx, events.RideStatusChangedEvent{
		RideRequestID:    updated.ID,
		VehicleID:        updated.VehicleID,
		RiderID:          updated.RiderID,
		OwnerID:          updated.OwnerID,
		Status:           updated.Status,
		RequesterName:    updated.RequesterName,
		RescheduleStatus: updated.RescheduleStatus,
		// Carried so the rider's push copy can tell a reservation from an
		// instant request (MYR-360); not projected onto the WS frame.
		ScheduledFor: updated.ScheduledFor,
		UpdatedAt:    updated.UpdatedAt,
	})

	return updated, true
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
