package telemetry

import (
	"fmt"
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// writeWindowConflict is the ONE place the MYR-383 refusal becomes a response,
// shared by the rider-facing CREATE and the owner-facing ACCEPT backstop. Both
// landing sites enforce one predicate in the store; both explain it with one
// body here, so the two can never disagree about what a client sees.
//
// THE CODE: `409 vehicle_unavailable`, reusing the code the three sibling
// capability refusals already carry (MYR-277 in-service/offline, MYR-342
// ride-share pause, MYR-266 already-on-a-ride) rather than minting a new one.
// It is precisely accurate — the request is well formed and the caller
// authorised, the vehicle simply cannot serve this ride — and it is NOT
// `conflict`, which means an illegal lifecycle transition on a known ride: the
// ride here is perfectly legal, the car is just spoken for at that hour. A new
// top-level code would also have to be added to the shared ErrorPayload.code
// enum every SDK compiles against, for a refusal an existing code describes.
//
// THE SUB-CODE: `time_conflict`, because the three siblings are all conditions
// of the car RIGHT NOW, which a client answers with "try again later", while
// this one is a property of the TIME the rider picked, which a client answers
// by sending them back to the picker. A client MUST NOT tell those apart by
// reading the message (§4.1 rule 1) — hence a typed sub-code.
//
// THE BODY: the conflicting INSTANT and nothing else. Naming it is what lets a
// client say "that slot is taken" the moment the rider taps, which is the whole
// ask; it is P0 operational timing, the same tier as `status` and as the
// service-window instant the MYR-316 refusal already echoes. The conflicting
// ride's id, rider, requester name, pickup and dropoff are P1 and belong to the
// OTHER party (data-classification.md §1.9) — the caller is not a party to that
// ride, and a booking probe must never become a way to read a stranger's
// calendar. Nothing here is logged: the refusal is an ordinary, expected
// outcome, not an operational event.
func (h *RideRequestHandler) writeWindowConflict(w http.ResponseWriter, conflict *RideWindowConflictError) {
	h.writeErrorSub(w, http.StatusConflict, wserrors.ErrCodeVehicleUnavailable,
		wserrors.SubCodeTimeConflict, windowConflictMessage(conflict))
}

// windowConflictMessage composes the refusal prose — three cases, each saying
// only what is TRUE. A nil instant is the ACTIVE-INSTANT arm: the car is on a
// ride right now, so there is no scheduled instant to name and the message says
// so instead of inventing one. A PENDING conflict is a request nobody has
// agreed to yet, so it must not be called "booked" — a rider told their slot is
// booked when it is merely contested would be misinformed, and this refusal
// exists because the client asked for accuracy.
func windowConflictMessage(c *RideWindowConflictError) string {
	switch {
	case c.ConflictAt == nil:
		return "Vehicle is already on a ride and can't also be booked for that time"
	case c.Pending:
		return fmt.Sprintf("Vehicle already has a ride request for %s",
			c.ConflictAt.UTC().Format(time.RFC3339))
	default:
		return fmt.Sprintf("Vehicle is already booked for a ride at %s",
			c.ConflictAt.UTC().Format(time.RFC3339))
	}
}
