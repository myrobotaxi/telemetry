// The MYR-581 nameless-owner hold, the third and last enforcement layer of the
// gate whose two request-time layers live in internal/telemetry's
// owner_name_gate.go. Split out of reservation_worker.go under the 300-line rule
// — and it reads better here anyway: the arm's whole justification is a
// monotonicity argument that has no counterpart in the probes it sits beside.

package dispatch

import "log/slog"

// holdIfOwnerNameless is the THIRD and last enforcement layer of the
// nameless-owner gate, and the only one that runs outside a request.
//
// WHY IT IS A BACKSTOP RATHER THAN A LIVE GATE, and why that makes it different
// in kind from the pause probe it sits immediately after: namelessness only ever
// resolves FORWARD. The profile writer refuses the empty string on every rung it
// touches, so no path in this server can un-name an account, and a car that
// becomes offerable stays offerable. What this layer catches is therefore
// reservations accepted BEFORE the create gate existed — a finite, shrinking
// population — not a condition that can come back. The full argument is in
// internal/telemetry/owner_name_gate.go, which owns the other two layers.
//
// HOLDS RATHER THAN EXPIRES, beside the pause arm and BEFORE the claim, and
// the mirror is exact: an owner who sets their name inside the lateness
// window gets the dispatch they meant to allow, exactly as an owner who
// un-pauses does; one who never does lets the reservation resolve honestly
// as `reservation_expired` at scheduledFor + MaxLateness, with no new
// resolution path and no new wire status.
//
// The NAME IS NEVER LOGGED and never read here — the store hands this layer
// a boolean, so there is no P1 value in the sweep at all.
func (s *ReservationSweeper) holdIfOwnerNameless(r *DueReservation, state VehicleDispatchState) bool {
	if state.OwnerNamed {
		return false
	}
	s.logger.Info("reservation sweep: vehicle owner has no display name, holding",
		slog.String("ride_id", r.RideRequestID),
		slog.String("vehicle_id", r.VehicleID),
		slog.Time("scheduled_for", r.ScheduledFor),
	)
	return true
}
