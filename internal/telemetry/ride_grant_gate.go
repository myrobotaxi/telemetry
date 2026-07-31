package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The MYR-369 RIDE-CAPABILITY BACKSTOP on the owner accept path.
//
// WHY AN ACCEPT-TIME CHECK EXISTS AT ALL. Before MYR-369 a viewer's ride access
// was fixed at invite time, so the create-time gate was the whole story: a
// request that existed had, by construction, been made by somebody who was
// allowed to make it, and nothing could change that between create and accept
// except a revoke. Capabilities are now EDITABLE IN PLACE, which opens a window
// the old model did not have — an owner who patches `allowRides` off, or
// suspends the grant, while a request from that rider is sitting unanswered in
// their queue. Without this check, accepting that request would dispatch a car
// to somebody the owner had just decided may not have it, and the owner would
// have done it themselves by tapping Accept.
//
// It is a BACKSTOP, not the gate: the create path already refuses, and the
// reservation sweeper refuses again at dispatch. Three layers, each covering the
// window the one before it cannot reach — the same shape as the MYR-342 pause.
//
// WHERE IT SITS. After the vehicle read that established status and the pause,
// so a caller with no business here still gets the answer they would have got
// anyway. It costs one extra query and only on the non-owner-rider path: a
// request the owner made for themselves short-circuits without touching the
// share store.
//
// FAILS CLOSED ON AN UNREADABLE GRANT, deliberately unlike the MYR-313
// fail-open above it. That exemption exists because refusing a reservation over
// an unreadable VEHICLE strands an owner over a condition that will clear on its
// own. An unreadable GRANT is not that: the question is whether this specific
// person is currently permitted, the answer is unknown, and dispatching a car to
// somebody who may have just been suspended is not recoverable the way a retried
// accept is.
//
// A NIL SHARE READER IS NOT "UNREADABLE" — it is a deployment with no sharing
// wired at all, and the backstop no-ops. That is not a hole: with no share
// subsystem there are no grants, so no capability can have been withdrawn, and
// the only way a non-owner rider row could exist is the create gate, which is
// itself owner-only under a nil reader. Denying here instead would refuse every
// accept on a sharing-less deployment over a capability nobody could have lost.

// rejectIfRideNotGranted writes a 403 and returns true when the RIDER on this
// request is not the vehicle's owner and no longer holds a grant carrying the
// ride capability.
//
// shares may be nil — see the file header for why that no-ops rather than
// denying.
func rejectIfRideNotGranted(
	ctx context.Context,
	w http.ResponseWriter,
	logger *slog.Logger,
	shares VehicleShareReader,
	riderID, vehicleID, vehicleOwnerID string,
) bool {
	if shares == nil {
		return false
	}
	if _, err := vehicleAccessFor(ctx, shares, riderID, vehicleID, vehicleOwnerID, capRides); err != nil {
		if errors.Is(err, errNoVehicleAccess) {
			// The RIDER is named, not the caller: the caller is the owner
			// and is not the one being refused. The message says nothing
			// about which capability moved — the owner already knows what
			// they changed, and the rider never sees this response.
			logger.Info("ride-request accept: rider no longer holds the ride capability",
				slog.String("vehicle_id", vehicleID),
				slog.String("rider_id", riderID),
			)
			wserrors.WriteErrorEnvelope(w, logger, http.StatusForbidden,
				wserrors.ErrCodeVehicleNotOwned,
				"this rider no longer has permission to request rides in this vehicle")
			return true
		}
		logger.Error("ride-request accept: grant lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("rider_id", riderID),
			slog.String("error", err.Error()),
		)
		wserrors.WriteErrorEnvelope(w, logger, http.StatusInternalServerError,
			wserrors.ErrCodeInternalError, "internal error")
		return true
	}
	return false
}
