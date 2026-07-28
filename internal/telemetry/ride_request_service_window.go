package telemetry

import (
	"fmt"
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// rejectIfBeforeServiceWindow is the MYR-316 scheduler bound: a SCHEDULED ride
// may not be booked for a time EARLIER than the vehicle is expected back from
// its current service visit.
//
// It closes the hole MYR-277 + MYR-313 left open. MYR-277 refuses to dispatch
// an in-service car; MYR-313 then made SCHEDULED accepts exempt from that gate,
// on the sound reasoning that a reservation days out says nothing about the
// car's status today. But "days out" was doing unexamined work: nothing stopped
// an owner accepting a ride scheduled for two hours from now while the car sat
// at a service centre until Friday, and the rider would simply be stood up. The
// service window is the missing fact, and this is where it is enforced.
//
// Three rules, all from contracts v0.17.0:
//
//   - INSTANT rides are unaffected. They are already refused outright for an
//     in-service car by the MYR-277 gate, so a floor on a time they do not have
//     would be meaningless.
//   - NO ESTIMATE MEANS NO BOUND. A null serviceEstimatedEndAt — including the
//     very common case where Tesla has no appointment record and the owner
//     never typed one — leaves scheduling fully open. Missing data must never
//     block a booking; the contract says so explicitly, and the alternative
//     would make an unreadable service_data silently un-bookable.
//   - EQUAL IS ALLOWED. The bound is strictly "earlier than", so a ride
//     scheduled for exactly the estimated end passes. The estimate is an
//     estimate; refusing the boundary instant would be false precision.
//
// The message NAMES the instant so the client can explain the refusal without a
// second round trip. That value is P0 (operational timing, the same tier as
// `status`), so echoing it in an error envelope is safe — contrast the P1 plate
// and place labels, which are never echoed.
//
// Returns true (response already written) when the request must stop.
func (h *RideRequestHandler) rejectIfBeforeServiceWindow(
	w http.ResponseWriter,
	scheduledFor *time.Time,
	row VehicleSnapshotRow,
) bool {
	if scheduledFor == nil {
		return false
	}
	end := resolveServiceEstimatedEndAt(row.Status, row.ServiceETC, row.ServiceExpectedEndAt)
	if end == nil {
		return false
	}
	if !scheduledFor.Before(*end) {
		return false
	}

	h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest,
		fmt.Sprintf("scheduledFor must not be earlier than the vehicle's estimated service end (%s)",
			end.UTC().Format(time.RFC3339)))
	return true
}
