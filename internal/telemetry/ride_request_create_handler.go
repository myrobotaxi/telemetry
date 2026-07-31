// The ride-request CREATE path. Split out of ride_request_handler.go (MYR-376)
// to bring that file back under the 300-line rule; nothing about the behaviour
// moves with it. Create is the natural seam — it is the only endpoint on this
// handler with its own multi-stage precondition chain (vehicle access, ride-
// sharing pause, service window, one-active-ride) and its own error mapper,
// while everything left behind is cancel plus the shared plumbing.

package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

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
	// MYR-369: the gate reads the grant's `allow_rides` FLAG, not a tier. A
	// viewer whose grant does not carry it is refused here even though they
	// can see the car — and the owner can now take the capability away in
	// place (PATCH /api/invites/{inviteId}) instead of revoking the whole
	// grant, so this check is live state rather than something fixed at
	// invite time. capRides carries the suspension term itself, so a
	// suspended grant is refused here as well as being absent from the
	// access set.
	if _, accessErr := vehicleAccessFor(ctx, h.shares, userID, in.VehicleID, row.UserID, capRides); accessErr != nil {
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
