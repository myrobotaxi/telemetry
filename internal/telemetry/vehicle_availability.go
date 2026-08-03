// The dispatch-availability predicate (MYR-277, generalised by MYR-372).
//
// "Can this car be dispatched RIGHT NOW?" is asked in two places that must
// never disagree: the owner-accept gate on an INSTANT ride, and — since
// MYR-372 — the reservation sweeper at a scheduled ride's due instant. Both
// are the same question about the same persisted column, asked at the moment
// a nav push is about to happen.
//
// It lives in ONE function for the reason service_window.go gives about its
// own two-rule precedence: a second copy would eventually disagree, and the
// copy that disagreed would be the sweeper — silently dialling a car the
// accept path would have refused, with no human in the loop to notice.
//
// The sweeper cannot import this package (it is a dispatch-layer concern with
// its own store seam), so the predicate is EXPORTED and applied by the
// cmd-level adapter that already spans both. The vehicle-status vocabulary
// therefore stays here, where the monitor that writes it lives, and never
// leaks into internal/store or internal/dispatch as a restated literal.

package telemetry

// vehicleAvailability answers the dispatch-capability question for one
// persisted vehicle status, returning BOTH the verdict and the rider-facing
// refusal that goes with it.
//
// Returning the pair from one switch is what makes the two readers exhaustive
// by construction: there is no status the predicate calls undispatchable and
// the accept path has no message for, and none the accept path refuses that
// the sweeper would happily push. `parked`/`driving`/`charging` are
// dispatchable and fall through.
//
// The refusal strings are P0 — a vehicle status, no plate, place or party — so
// they are safe to echo to whoever is holding the request.
func vehicleAvailability(status string) (dispatchable bool, refusal string) {
	switch status {
	case serviceStatusInService:
		// The owner put the car into service. It cannot fulfill a ride.
		return false, "Vehicle is in service and can't be dispatched"
	case vehicleStatusOffline:
		// The server cannot currently reach the car, so a nav push would go
		// nowhere.
		return false, "Vehicle is offline and can't be dispatched"
	}
	return true, ""
}

// VehicleStatusDispatchable reports whether a vehicle in this persisted status
// can be dispatched right now. Exported for the reservation sweeper's adapter
// (MYR-372), which asks the accept path's question at the reservation's due
// instant — the moment MYR-313's exemption was always meant to defer it to.
func VehicleStatusDispatchable(status string) bool {
	ok, _ := vehicleAvailability(status)
	return ok
}
