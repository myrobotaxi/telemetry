package telemetry

// MYR-507 vehicle-IDENTITY backfill, hung off the SAME non-waking read the
// MYR-320 colour write already rides (connectivity edge or the periodic
// in-service pass).
//
// It sits beside syncVehicleColor in service_status_vehicle_details.go for one
// reason and differs from it in one way:
//
//   - SAME: `model` and `year` are EXISTING wire fields fed by EXISTING Prisma
//     columns that nothing ever populated for a Go-provisioned car, so like
//     `color` they take a narrow column UPDATE and never travel as telemetry
//     fields. Filling them reaches every consumer with zero contract change.
//   - DIFFERENT: this needs NOTHING from Tesla. Both values come from the VIN,
//     which the monitor already has in hand. The Fleet API carries no model year
//     at any path, so the VIN is the only source of one anyway.
//
// That difference is why the write is worth doing here even though provisioning
// now seeds both columns (store.OwnerProvisioner): the seed only helps cars
// linked from now on. This pass is what repairs the cars ALREADY in the table —
// including the shared car the field report was written about, which had been
// unidentifiable for hours.
//
// Optional, like every other enrichment on this monitor: omit the wiring option
// and the monitor behaves exactly as it did before, which is what the
// direct-handler unit tests rely on.

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/vin"
)

// VehicleIdentityWriter fills the VIN-derived model / model-year columns on the
// Prisma-owned "Vehicle" row for a car the given user owns. Satisfied by
// *store.VehicleRepo.
//
// Declared at the consumer site and kept OFF the required reader interfaces, so
// a deployment without the option wired simply never issues the write.
//
// Ownership is a parameter, not a caller precondition: the implementation puts
// it in the WHERE clause so a mismatched owner updates ZERO rows rather than
// another owner's car. Implementations MUST fill only EMPTY columns — never
// overwrite a model or year some other writer already got right — and MUST
// treat an empty model and a zero year as "nothing to say", not as a clear.
type VehicleIdentityWriter interface {
	FillVehicleIdentity(ctx context.Context, userID, vin, model string, year int) (bool, error)
}

// WithVehicleIdentity enables the MYR-507 model / model-year backfill. Without
// it the monitor writes neither column and a Go-provisioned car keeps whatever
// the provisioning INSERT left behind — for cars linked before MYR-507, the
// placeholders `”` and `0`.
func WithVehicleIdentity(writer VehicleIdentityWriter) ServiceStatusMonitorOption {
	return func(m *ServiceStatusMonitor) {
		if writer != nil {
			m.vehicleIdentity = writer
		}
	}
}

// syncVehicleIdentity fills this car's `model` and `year` from its VIN — the
// fifth sanctioned Go-side write carve-out on the Prisma-owned "Vehicle" table
// (data-lifecycle.md §1.4), after provision, teardown, the plate and the colour.
//
// FILL-IF-EMPTY at both layers. A car whose columns are already populated
// matches zero rows, so for all but the first pass over a given vehicle this is
// one no-op statement — and the Prisma web-link flow's richer model
// ("Model 3 Performance") is never downgraded to what position 4 of a VIN can
// encode ("Model 3").
//
// An UNRECOGNISED VIN code yields the zero value from `internal/vin` and is
// never written: "we could not read it" must not blank a value that is right.
//
// Every failure is non-fatal and logged. Identity is cosmetic in the same sense
// the colour is, and losing it must never disturb the control-field backfill it
// rides along with.
func (m *ServiceStatusMonitor) syncVehicleIdentity(ctx context.Context, vehicleVIN string) {
	if m.vehicleIdentity == nil {
		return
	}
	model, year := vin.Model(vehicleVIN), vin.ModelYear(vehicleVIN)
	if model == "" && year == 0 {
		// Neither position decoded. Nothing to say, so nothing is written and
		// the owner lookup below is skipped entirely.
		m.logger.Debug("vehicle-identity: VIN encodes no recognisable model or year",
			slog.String("vin", redactVIN(vehicleVIN)))
		return
	}

	userID, err := m.owners.GetVehicleOwner(ctx, vehicleVIN)
	if err != nil {
		m.logger.Warn("vehicle-identity: owner lookup failed — identity not written (non-fatal)",
			slog.String("vin", redactVIN(vehicleVIN)), slog.String("error", err.Error()))
		return
	}

	filled, err := m.vehicleIdentity.FillVehicleIdentity(ctx, userID, vehicleVIN, model, year)
	if err != nil {
		m.logger.Warn("vehicle-identity: identity write failed (non-fatal)",
			slog.String("vin", redactVIN(vehicleVIN)), slog.String("error", err.Error()))
		return
	}
	if !filled {
		// The ordinary outcome, and the one this path reaches on every pass
		// after the first: both columns already hold a value. Indistinguishable
		// from an unknown VIN or a car owned by somebody else, all three of
		// which are information rather than errors.
		m.logger.Debug("vehicle-identity: nothing to fill",
			slog.String("vin", redactVIN(vehicleVIN)), slog.String("user_id", userID))
		return
	}
	m.logger.Info("vehicle-identity: model/year populated from VIN",
		slog.String("vin", redactVIN(vehicleVIN)),
		slog.String("model", model),
		slog.Int("year", year))
}
