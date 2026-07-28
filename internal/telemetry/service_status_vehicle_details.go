package telemetry

// MYR-320 vehicle-DETAIL enrichment, hung off the SAME non-waking read the
// MYR-260 /vehicle_data backfill already performs (connectivity edge or the
// MYR-320 periodic in-service pass). Two values live here because neither fits
// the vehicle_data → telemetry-field pipeline in service_status_vehicle_data.go:
//
//   - fsdVersion needs a SECOND Fleet API endpoint (release_notes) — no
//     vehicle_data field carries it — so it is fetched here and then injected
//     into the same synthetic frame, reaching the same control-state persist.
//   - color has an EXISTING wire field fed by an EXISTING Prisma column that
//     nothing ever populated, so it takes a narrow column UPDATE and never
//     travels as a telemetry field at all.
//
// Both are optional: omit the wiring option and the monitor behaves exactly as
// it did before MYR-320, which is what the direct-handler unit tests rely on.

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// ReleaseNotesReader reads a vehicle's release-notes list. Satisfied by
// *FleetAPIClient.GetReleaseNotes. Declared at the consumer site — and kept OFF
// the required FleetVehicleReader interface — so the monitor stays unit-testable
// with a fake and so a deployment without the option wired simply never issues
// the call. NO real Tesla call ever fires from a test.
type ReleaseNotesReader interface {
	GetReleaseNotes(ctx context.Context, token, vin string) ([]ReleaseNote, error)
}

// VehicleColorWriter persists Tesla's exterior-colour name onto the
// Prisma-owned "Vehicle".color column for a car the given user owns. Satisfied
// by *store.VehicleRepo.
//
// Ownership is a parameter, not a caller precondition: the implementation puts
// it in the WHERE clause so a mismatched owner updates ZERO rows rather than
// another owner's car. Implementations MUST reject an empty colour rather than
// blanking a column that already holds a good value.
type VehicleColorWriter interface {
	UpdateVehicleColor(ctx context.Context, userID, vin, color string) (bool, error)
}

// WithReleaseNotes enables the MYR-320 fsdVersion read. Without it the monitor
// issues no release_notes call and fsdVersion stays null forever — the
// pre-MYR-320 behaviour.
func WithReleaseNotes(reader ReleaseNotesReader) ServiceStatusMonitorOption {
	return func(m *ServiceStatusMonitor) {
		if reader != nil {
			m.releaseNotes = reader
		}
	}
}

// WithVehicleColor enables the MYR-320 exterior-colour column write. Without it
// the monitor decodes exterior_color and discards it, leaving "Vehicle".color at
// whatever the provisioning placeholder ('') left behind.
func WithVehicleColor(writer VehicleColorWriter) ServiceStatusMonitorOption {
	return func(m *ServiceStatusMonitor) {
		if writer != nil {
			m.vehicleColor = writer
		}
	}
}

// addFSDVersionField issues ONE extra non-waking GET
// /api/1/vehicles/{vin}/release_notes and, when the newest entry carries a
// non-empty title, adds it to the frame as FieldFSDVersion so it rides the
// identical broadcast + control-state persist path as its REST-only siblings
// `trim` and `trimLabel`.
//
// Every failure mode is the SAME no-op: option not wired, no token, read error,
// empty list, absent or empty title. In each case no field is added and the
// stored value is left alone — an unreadable release-notes endpoint must never
// look like "this car lost its FSD version". The read error stays at Debug for
// the same reason the vehicle_data read does: 408 from a sleeping car is the
// ordinary outcome for a parked fleet, not an operator concern.
//
// Being absent from fieldMap is what carries the field past the MYR-300
// stream-recency gate (see service_status_stream_freshness.go): Tesla streams no
// FSD designation, so the stream can never be its source and there is no
// stale-overwrite to guard against. That matters most for the case MYR-320
// exists for — an in-service car, which never streams at all.
func (m *ServiceStatusMonitor) addFSDVersionField(
	ctx context.Context,
	vin, token string,
	fields map[string]events.TelemetryValue,
) {
	if m.releaseNotes == nil || token == "" {
		return
	}

	notes, err := m.releaseNotes.GetReleaseNotes(ctx, token, vin)
	if err != nil {
		m.logger.Debug("vehicle-details: release_notes read skipped (non-fatal, no wake)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return
	}

	title := newestReleaseTitle(notes)
	if title == "" {
		// A car with no notes, or a note with no title. Common and normal — it
		// is NOT a claim that the car lacks FSD, so nothing is written.
		m.logger.Debug("vehicle-details: no release-notes title to record",
			slog.String("vin", redactVIN(vin)), slog.Int("notes", len(notes)))
		return
	}

	fields[string(FieldFSDVersion)] = events.TelemetryValue{StringVal: &title}
}

// syncVehicleColor writes Tesla's exterior-colour name onto the Prisma-owned
// "Vehicle".color column — the fourth sanctioned Go-side write carve-out on that
// table (data-lifecycle.md §1.4), after provision, teardown and the license
// plate.
//
// Why a carve-out: `color` is already a wire field on BOTH the snapshot and the
// vehicles-list, already emitted straight from this column — and the column has
// only ever held the empty placeholder the MYR-257 provisioning INSERT seeds,
// because no writer anywhere populated it. Filling it in makes the value flow to
// every consumer with ZERO contract change, which is precisely why this is a
// column write and not a new field.
//
// An EMPTY or absent exterior_color is NEVER written. Tesla omitting the key on
// a partial payload must not blank a colour an earlier read (or a human) already
// got right — the same empty-string-skip rule the sibling `trim` follows, and
// the reason the guard lives here as well as in the store.
//
// Every failure is non-fatal and logged: a colour is cosmetic, and losing it must
// never disturb the control-field backfill it rides along with.
func (m *ServiceStatusMonitor) syncVehicleColor(ctx context.Context, vin string, vc *VehicleDataVehicleConfig) {
	if m.vehicleColor == nil || vc == nil || vc.ExteriorColor == nil || *vc.ExteriorColor == "" {
		return
	}
	color := *vc.ExteriorColor

	userID, err := m.owners.GetVehicleOwner(ctx, vin)
	if err != nil {
		m.logger.Warn("vehicle-details: owner lookup failed — colour not written (non-fatal)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return
	}

	updated, err := m.vehicleColor.UpdateVehicleColor(ctx, userID, vin, color)
	if err != nil {
		m.logger.Warn("vehicle-details: colour write failed (non-fatal)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return
	}
	if !updated {
		// Zero rows matched: an unknown VIN, or one owned by somebody else. The
		// ownership predicate is doing its job, so this is information, not an
		// error.
		m.logger.Debug("vehicle-details: colour write matched no owned row",
			slog.String("vin", redactVIN(vin)), slog.String("user_id", userID))
		return
	}
	m.logger.Info("vehicle-details: exterior colour populated from vehicle_config",
		slog.String("vin", redactVIN(vin)), slog.String("color", color))
}
