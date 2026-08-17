package telemetry

// The per-row PROJECTION for GET /api/vehicles: the one function that turns a
// VehicleCatalogRow into the wire shape, plus its VIN helper.
//
// Third file in the §7.0 trio, split out under the CLAUDE.md 300-line cap when
// MYR-507/MYR-515 added two fields: vehicles_list_handler.go holds the request
// flow, vehicles_list_types.go holds the shapes, and this holds the mapping
// between them. Keeping the builder on its own means the next field added to
// the catalog lands in a file with room for the argument that justifies it —
// which, on this surface, has been most of the diff every time.
//
// Shared by all three §7.0 producers (the owner list, the viewer merge, and the
// redeem response), so they cannot emit different rows for the same vehicle.

import (
	"github.com/myrobotaxi/telemetry/internal/trim"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// newVehicleSummary builds the per-row wire shape for a catalog row under a
// given role. grant is the caller's share capability set and is read ONLY on
// viewer rows — pass the zero ShareGrant for an owner, who holds no grant.
//
// Shared by the owner list, the viewer merge (vehicles_list_viewer.go), and the
// redeem response, so all three emit byte-identical rows for the same vehicle.
//
// now is the instant the MYR-491 setup derivation is judged against, passed in
// rather than read here for the same two reasons as on the snapshot: one
// response is one instant, and the rules are testable without waiting.
func newVehicleSummary(v *VehicleCatalogRow, role auth.Role, grant auth.ShareGrant, now time.Time) vehicleSummary {
	summary := vehicleSummary{
		VehicleID:      v.ID,
		Name:           v.Name,
		Model:          v.Model,
		Year:           v.Year,
		Color:          v.Color,
		LicensePlate:   v.LicensePlate,
		VinLast4:       lastFourOfVIN(v.VIN),
		Status:         v.Status,
		ChargeLevel:    v.ChargeLevel,
		EstimatedRange: v.EstimatedRange,
		LastUpdated:    v.LastUpdated.UTC().Format(time.RFC3339),
		Role:           string(role),
		HasActiveRide:  v.HasActiveRide,
		// MYR-316: resolved here so the precedence and the in-service gate
		// are applied exactly once per surface (service_window.go).
		ServiceEstimatedEndAt: serviceEstimatedEndAtWire(v.Status, v.ServiceETC, v.ServiceExpectedEndAt),
		// MYR-342: emitted verbatim on BOTH roles. The mask, not this
		// assignment, is what decides who sees it — and both allow-lists carry
		// it, because a rider who cannot see that a shared car is paused finds
		// out from a 409 instead.
		RideShareEnabled: v.RideShareEnabled,
		// MYR-507/578: RESOLVED, on BOTH roles — Tesla's own display-safe
		// label first, else the badge code, else the VIN's drive-unit digit
		// (`internal/trim`). The snapshot runs the identical resolver over the
		// identical inputs, so the two surfaces still cannot name one car two
		// different ways. Identity, not telemetry — see the mask allow-list
		// for why a viewer sees it.
		TrimLabel: resolvedTrimLabel(v.Model, v.Year, v.TrimLabel, v.Trim, v.VIN),
		// MYR-581: carried verbatim on BOTH roles. The ladder ran in the store
		// (three identity sources, one statement per catalog read) and the
		// first-token reduction ran there too, reusing MYR-229's `firstNameToken`
		// rather than re-implementing it — so this surface, the ride card's
		// `requesterName` and the redeem screen's `ownerFirstName` all shorten a
		// name the same way, and there is no second rule to drift.
		OwnerFirstName: v.OwnerFirstName,
		// MYR-515: resolved here so the atomic-pair rule and the (0,0)
		// sentinel collapse are applied exactly once per surface, in the same
		// place the MYR-316 window resolves its precedence.
		Location: newVehicleLocation(v.Latitude, v.Longitude),
		// MYR-491: the SAME derivation the snapshot runs, over the same inputs.
		// Calling it from both surfaces rather than copying the rules is what
		// makes "the catalog and the detail sheet can never disagree" a
		// structural property instead of a convention.
		SetupState: deriveSetupState(now, v.Status, v.LastUpdated, v.SetupSchedule),
	}
	if role == auth.RoleViewer {
		// DERIVED, not stored (MYR-369): the grant's flags decide the
		// compatibility value a pre-MYR-369 client reads. A suspended grant
		// never reaches here — the viewer's catalog query excludes it, so
		// the vehicle is absent from the response entirely rather than
		// present with some reduced value.
		summary.SharePermission = grant.Permission().String()
	}
	return summary
}

// lastFourOfVIN returns the last 4 characters of a VIN. Empty input
// yields an empty string; shorter-than-4 inputs are returned verbatim
// (the contract guarantees 17-char VINs, but the helper is defensive
// against test fixtures that pass shorter values).
func lastFourOfVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return vin[len(vin)-4:]
}

// resolvedTrimLabel adapts internal/trim.Resolve's string answer to the wire's
// nullable pointer: "" — no honest trim — is an explicit JSON null, never an
// empty fragment in a descriptor.
func resolvedTrimLabel(model string, year int, storedLabel, badge *string, vin string) *string {
	label := trim.Resolve(model, year, derefTrim(storedLabel), derefTrim(badge), vin)
	if label == "" {
		return nil
	}
	return &label
}

func derefTrim(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
