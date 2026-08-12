// Guarded ride-request status transitions at the HTTP layer. Split out of
// ride_request_handler.go (MYR-376) so the two variants — the plain guarded
// write and the reservation-dormancy-guarded one behind the owner pickup —
// sit beside each other and share ONE error-mapping + publish path. They
// differ only in WHICH store write they call; everything a client can observe
// about a refusal or a success is decided here, once.

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

// rideStatusWriter is one of the store's GUARDED transition writes — a single
// UPDATE whose WHERE carries the transition precondition, so the database (not
// a pre-check read) decides. RideRequestStore.UpdateStatusFrom and
// UpdateStatusFromDispatched both satisfy it.
type rideStatusWriter func(ctx context.Context, id string, from []string, to string) (RideRequestData, error)

// mutateStatus persists a status change ATOMICALLY via the store's guarded
// UpdateStatusFrom — a single UPDATE with `WHERE status = ANY(from)` — so a
// check-then-write race (rider-cancel vs owner-decline, owner double-accept)
// resolves in the database: exactly one caller wins; the loser gets a 409
// conflict here even if its pre-check read passed. Only the winning call
// publishes the ride_status_changed summary event (and, for accept, the
// ride.accepted dispatch seam at the call site) — exactly-once by
// construction. Does NOT write the success response; the caller does. On
// failure writes the error envelope and returns ok=false. Shared by cancel
// (MYR-174), accept/decline (MYR-175), start and dropped-off (MYR-270).
func (h *RideRequestHandler) mutateStatus(ctx context.Context, w http.ResponseWriter, rec RideRequestData, from []string, to string) (RideRequestData, bool) {
	return h.mutateStatusWith(ctx, w, rec, from, to, h.store.UpdateStatusFrom)
}

// mutateStatusCancelled is mutateStatus over the store's INITIATOR-STAMPING
// cancel write (MYR-522): the same guarded UPDATE, with `cancelled_by` written
// first-writer-wins in the same statement. It backs BOTH cancel paths — the
// rider's and the owner's — which differ only in their allowed-from set and in
// the party they stamp, so neither can drift in how a cancel is persisted or
// published.
func (h *RideRequestHandler) mutateStatusCancelled(ctx context.Context, w http.ResponseWriter, rec RideRequestData, from []string, by string) (RideRequestData, bool) {
	return h.mutateStatusWith(ctx, w, rec, from, rideStatusCancelled,
		func(ctx context.Context, id string, from []string, _ string) (RideRequestData, error) {
			return h.store.UpdateStatusFromCancelled(ctx, id, from, by)
		})
}

// mutateStatusDispatched is mutateStatus over the store's DORMANCY-GUARDED
// write (MYR-376), and it backs exactly one caller: the owner pickup
// transition (accepted → arrived).
//
// The extra precondition — the ride is not a reservation that is still both
// undispatched and not yet due — rides in the SAME UPDATE as the status guard,
// so it is arbitrated by the database on identical terms: a sweeper dispatching
// concurrently with a pickup tap (or the clock crossing `scheduledFor` under
// one) resolves consistently, and no pickup can commit against a row that is
// dormant at the instant of the write. The
// refusal reuses the `conflict` code (rest-api.md §7.8); only the message
// differs, so it can name the reason rather than blame the status.
func (h *RideRequestHandler) mutateStatusDispatched(ctx context.Context, w http.ResponseWriter, rec RideRequestData, from []string, to string) (RideRequestData, bool) {
	return h.mutateStatusWith(ctx, w, rec, from, to, h.store.UpdateStatusFromDispatched)
}

// mutateStatusUnconflicted is mutateStatus over the store's BOOKING-LOCKED
// write (MYR-383), and it backs exactly one caller: the owner ACCEPT transition
// (requested → accepted).
//
// The extra precondition — the ride's target vehicle is not already promised to
// another open ride inside the booking window — is evaluated inside the SAME
// transaction as the status guard, under a per-vehicle advisory lock, so two
// owners confirming conflicting reservations for one car are arbitrated by the
// database rather than by whichever pre-check read happened to be stale. No
// accept can commit against a window that is taken at the instant of the write.
//
// This is the BACKSTOP half of the two-layer gate — the create-time refusal is
// the rider-facing half — following the MYR-316 service-window pattern exactly.
// It is not redundant: reservations booked before the gate existed can still be
// mutually conflicting, and a car that was idle at booking time may be mid-ride
// by the time the owner taps accept.
//
// The refusal is a *RideWindowConflictError, mapped by writeTransitionError to the
// same 409 `vehicle_unavailable` / `time_conflict` body the create path emits.
func (h *RideRequestHandler) mutateStatusUnconflicted(ctx context.Context, w http.ResponseWriter, rec RideRequestData, from []string, to string) (RideRequestData, bool) {
	return h.mutateStatusWith(ctx, w, rec, from, to, h.store.UpdateStatusFromUnconflicted)
}

// mutateStatusWith runs one guarded transition write and maps its outcome onto
// the wire. Every refusal a client can see is decided by `write` — the
// database — never by a read taken before it.
func (h *RideRequestHandler) mutateStatusWith(
	ctx context.Context, w http.ResponseWriter, rec RideRequestData, from []string, to string, write rideStatusWriter,
) (RideRequestData, bool) {
	updated, err := write(ctx, rec.ID, from, to)
	if err != nil {
		h.writeTransitionError(w, rec.ID, to, err)
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
		// Nil on every transition but a cancel — read off the UPDATED row, so
		// the initiator the push notifier sees is the one the guarded write
		// stamped (MYR-522).
		CancelledBy: updated.CancelledBy,
		UpdatedAt:   updated.UpdatedAt,
	})

	return updated, true
}

// writeTransitionError maps a guarded-write failure onto the REST error
// envelope. Both 409 causes on the pickup path — an illegal from-status and a
// dormant reservation — carry the SAME `conflict` code (MYR-376 deliberately
// adds no new error code); only the message distinguishes them.
func (h *RideRequestHandler) writeTransitionError(w http.ResponseWriter, id, to string, err error) {
	var window *RideWindowConflictError
	if errors.As(err, &window) {
		// MYR-383: the transition is legal and the ride is fine — the target
		// vehicle is already promised to another open ride within the booking
		// window. Same capability class as ErrVehicleRideActive below, and the
		// same body the create path emits (ride_window_conflict.go).
		h.writeWindowConflict(w, window)
		return
	}
	switch {
	case errors.Is(err, ErrVehicleRideActive):
		// Per-vehicle one-active-ride guard (MYR-266): the transition is
		// legal for THIS ride, but the target vehicle is already committed
		// to another active ride — a capability gate on the vehicle, the
		// same 409 vehicle_unavailable class as the MYR-277 in-service /
		// offline gate (the car cannot serve a second ride at once), NOT the
		// illegal-transition `conflict`.
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable, "Vehicle is already on another ride")
	case errors.Is(err, ErrRideReservationDormant):
		// MYR-376: the ride is still legally `accepted`, but it is a
		// reservation that is neither dispatched nor yet due — the car has been
		// told nothing and the ride is not owed yet, so there is no pickup to
		// confirm. Same `conflict` code as the illegal-transition case,
		// different message.
		h.writeError(w, http.StatusConflict, wserrors.ErrCodeConflict, "a scheduled ride cannot be picked up before its dispatch")
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
			slog.String("ride_request_id", id),
			slog.String("status", to),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
	}
}
