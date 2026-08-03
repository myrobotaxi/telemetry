// The dispatch-availability predicate (MYR-277, generalised by MYR-372).
//
// "Can this car be dispatched?" is asked in two places that must never
// disagree about the VOCABULARY — the owner-accept gate on an INSTANT ride,
// and the reservation sweeper at a scheduled ride's due instant. One switch
// enumerates the blocking statuses and their rider-facing refusals; each
// reader names which arms it acts on. A second copy of the enumeration would
// eventually disagree, and the copy that disagreed would be the sweeper —
// running unattended, with no 409 for anyone to notice.
//
// THE TWO READERS ACT ON DIFFERENT ARMS, DELIBERATELY. Accept refuses on both
// `in_service` and `offline`; the sweeper holds on `in_service` ONLY. That
// asymmetry is load-bearing and is argued in full at holdIfVehicleInService in
// internal/dispatch/reservation_worker.go. Read it before "fixing" the
// difference: making the sweeper hold on `offline` too silently strands every
// car that has not yet streamed telemetry.
//
// The sweeper cannot import this package (it is a dispatch-layer concern with
// its own store seam), so its arm is EXPORTED and applied by the cmd-level
// adapter that already spans both. The vehicle-status vocabulary therefore
// stays here, where the monitor that writes it lives, and never leaks into
// internal/store or internal/dispatch as a restated literal.

package telemetry

// vehicleBlocker names WHY a vehicle cannot be dispatched, so the two readers
// can act on different arms without either restating the status strings.
type vehicleBlocker int

const (
	// blockerNone: `parked`/`driving`/`charging`, and any status this build
	// does not recognise. The blocked set is enumerated, never inferred — a
	// value some future migration adds must not silently ground a fleet.
	blockerNone vehicleBlocker = iota
	// blockerInService: the owner put the car into service. Nothing a dispatch
	// does can clear this, and the MYR-320 periodic re-poll refreshes it
	// independently, so WAITING on it is a bounded, self-resolving wait.
	blockerInService
	// blockerOffline: the server has not seen the car. NOT symmetric with
	// in-service — nothing re-derives it, and the nav push's own wake ladder
	// is what clears it. See the file header and holdIfVehicleInService.
	blockerOffline
)

// vehicleAvailability classifies one persisted vehicle status, returning BOTH
// the blocker and the rider-facing refusal that goes with it.
//
// Returning the pair from one switch makes the accept gate exhaustive by
// construction: there is no blocked status it has no message for. The refusal
// strings are P0 — a vehicle status, no plate, place or party — so they are
// safe to echo to whoever is holding the request.
func vehicleAvailability(status string) (vehicleBlocker, string) {
	switch status {
	case serviceStatusInService:
		return blockerInService, "Vehicle is in service and can't be dispatched"
	case vehicleStatusOffline:
		return blockerOffline, "Vehicle is offline and can't be dispatched"
	}
	return blockerNone, ""
}

// VehicleStatusIsInService reports whether this persisted status is the
// in-service arm — the ONLY arm the reservation sweeper acts on (MYR-372).
//
// Exported rather than re-derived in internal/dispatch so the `in_service`
// literal keeps living in exactly one place. Why the sweeper stops at this arm
// is argued where the probe calling it lives, not here.
func VehicleStatusIsInService(status string) bool {
	blocker, _ := vehicleAvailability(status)
	return blocker == blockerInService
}
