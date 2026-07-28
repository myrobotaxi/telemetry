package telemetry

import "time"

// Service-window resolution (MYR-316). One function decides what
// `serviceEstimatedEndAt` is on EVERY surface that emits or enforces it — the
// REST /snapshot (VehicleState), the GET /api/vehicles list rows
// (VehicleSummary), and the scheduled-ride bound on create and accept. Sharing
// it is the point: three copies of a two-rule precedence would eventually
// disagree, and the one that disagreed would be the scheduler, silently
// admitting rides the UI had already told the rider were unavailable.
//
// The two rules, both from contracts v0.17.0:
//
//  1. SOURCE PRECEDENCE — Tesla's own estimate wins, the owner-entered value is
//     the fallback, null when neither exists. Kept as two columns so a late
//     Tesla estimate does not erase what the owner typed and a withdrawn one
//     falls back rather than to null.
//
//  2. IN-SERVICE ONLY — a vehicle whose status is not `in_service` NEVER
//     carries a value, even if the columns still hold one. The monitor also
//     physically clears both columns when it observes the car leaving service,
//     so this is belt-and-braces: it makes the wire honest in the window
//     between the car leaving service and the monitor noticing, and it means a
//     stale row can never leak an estimate.

// resolveServiceEstimatedEndAt applies both rules and returns the emitted
// instant, or nil when there is none. status is the vehicle's persisted status
// enum value.
func resolveServiceEstimatedEndAt(status string, teslaETC, ownerEnd *time.Time) *time.Time {
	if status != serviceStatusInService {
		return nil
	}
	if teslaETC != nil {
		return teslaETC
	}
	return ownerEnd
}

// serviceEstimatedEndAtWire is resolveServiceEstimatedEndAt rendered for the
// wire: an RFC 3339 UTC string, or nil for JSON null.
func serviceEstimatedEndAtWire(status string, teslaETC, ownerEnd *time.Time) *string {
	return formatInstantOrNil(resolveServiceEstimatedEndAt(status, teslaETC, ownerEnd))
}

// formatInstantOrNil renders an optional instant as RFC 3339 UTC, preserving
// nil so the field marshals to an explicit JSON null rather than an empty
// string. Matches the `lastUpdated` formatting on the same shapes.
func formatInstantOrNil(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
