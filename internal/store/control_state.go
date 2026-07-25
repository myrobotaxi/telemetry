package store

import (
	"context"
	"fmt"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// ControlStateUpdate holds the four owner-control read-back values MYR-269
// persists to the Go-owned go_vehicle_control_state side table: lock, trunk +
// frunk, climate, and charge-port state. Every field is a *bool: a nil pointer
// means "this frame did not carry the value", so the upsert leaves the stored
// column untouched (last-writer-wins PER FIELD). A non-nil false is a real
// observation and DOES overwrite.
//
// These are the same MYR-252 cabin read-backs the live WebSocket stream carries
// as `locked`, `frunkOpen`/`trunkOpen`, `isClimateOn`, and `chargePortDoorOpen`;
// MYR-269 makes them durable so a /snapshot for a non-streaming car returns the
// last-known value instead of a perpetual "unavailable".
type ControlStateUpdate struct {
	IsLocked       *bool
	FrunkOpen      *bool
	TrunkOpen      *bool
	IsClimateOn    *bool
	ChargePortOpen *bool
}

// HasAny reports whether at least one control field is present. The writer
// skips the side-table upsert entirely when a telemetry frame carries none of
// the four controls, so an ordinary speed/location frame never touches the
// table.
func (c *ControlStateUpdate) HasAny() bool {
	if c == nil {
		return false
	}
	return c.IsLocked != nil ||
		c.FrunkOpen != nil ||
		c.TrunkOpen != nil ||
		c.IsClimateOn != nil ||
		c.ChargePortOpen != nil
}

// mergeControlState folds the non-nil fields of src onto dst (latest wins per
// field), mirroring mergeUpdate's per-field last-write-wins for the Vehicle
// table. Both dst and src are non-nil.
func mergeControlState(dst, src *ControlStateUpdate) {
	dst.IsLocked = mergePtr(dst.IsLocked, src.IsLocked)
	dst.FrunkOpen = mergePtr(dst.FrunkOpen, src.FrunkOpen)
	dst.TrunkOpen = mergePtr(dst.TrunkOpen, src.TrunkOpen)
	dst.IsClimateOn = mergePtr(dst.IsClimateOn, src.IsClimateOn)
	dst.ChargePortOpen = mergePtr(dst.ChargePortOpen, src.ChargePortOpen)
}

// mapTelemetryToControlState derives the four owner-control booleans from a
// telemetry field map, using the SAME internal field names the protobuf decoder
// and the MYR-260 /vehicle_data backfill emit, and the SAME derivation the WS
// broadcast layer applies (internal/ws/field_mapping.go, door_fields.go) so a
// persisted value can never disagree with the live wire value:
//
//   - locked (bool)              → IsLocked
//   - doorState (bitmask int)    → FrunkOpen / TrunkOpen (DoorFrunk / DoorTrunk bits)
//   - hvacPower (enum string)    → IsClimateOn: "Off" ⇒ false; "On"/"Precondition"/
//     "OverheatProtect" ⇒ true; "Unknown" ⇒ OMITTED (nil) so a genuinely-unknown
//     climate never overwrites a known value with a fabricated on/off (MYR-251/252)
//   - chargePortDoorOpen (bool)  → ChargePortOpen
//
// Fields marked Invalid by the vehicle are ignored (the four controls are not
// atomic-group / clear-on-invalid fields). Returns nil when no control field is
// present so callers can cheaply skip the side-table write.
func mapTelemetryToControlState(fields map[string]events.TelemetryValue) *ControlStateUpdate {
	c := &ControlStateUpdate{}

	if v, ok := fields[string(telemetry.FieldLocked)]; ok && !v.Invalid && v.BoolVal != nil {
		locked := *v.BoolVal
		c.IsLocked = &locked
	}

	if v, ok := fields[string(telemetry.FieldDoorState)]; ok && !v.Invalid && v.IntVal != nil {
		bits := *v.IntVal
		frunk := events.DoorOpen(bits, events.DoorFrunk)
		trunk := events.DoorOpen(bits, events.DoorTrunk)
		c.FrunkOpen = &frunk
		c.TrunkOpen = &trunk
	}

	if v, ok := fields[string(telemetry.FieldHvacPower)]; ok && !v.Invalid && v.StringVal != nil {
		if on, known := climateOnFromHvacPower(*v.StringVal); known {
			c.IsClimateOn = &on
		}
	}

	if v, ok := fields[string(telemetry.FieldChargePortDoorOpen)]; ok && !v.Invalid && v.BoolVal != nil {
		open := *v.BoolVal
		c.ChargePortOpen = &open
	}

	if !c.HasAny() {
		return nil
	}
	return c
}

// climateOnFromHvacPower maps the HvacPowerState enum string to the derived
// isClimateOn boolean, matching internal/ws/field_mapping.go. The second return
// is false for "Unknown" (and any unrecognized value), signalling the caller to
// OMIT isClimateOn rather than assert a value. Comparison is case-insensitive to
// match the WS layer's strings.EqualFold usage.
func climateOnFromHvacPower(power string) (on, known bool) {
	switch {
	case equalFoldASCII(power, "Off"):
		return false, true
	case equalFoldASCII(power, "Unknown"):
		return false, false
	default:
		// "On" / "Precondition" / "OverheatProtect" (and any future non-Off,
		// non-Unknown state) mean the climate system is running.
		return true, true
	}
}

// equalFoldASCII is a tiny case-insensitive compare for the fixed HvacPowerState
// enum tokens. Kept local so control_state.go does not pull in strings just for
// two comparisons.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// queryUpsertControlState upserts the owner-control side-table row for one
// vehicle. Each control column uses COALESCE(EXCLUDED.col, existing.col): a NULL
// bind (field absent from this frame) keeps the stored value, a non-NULL bind
// (a real observation, including false) overwrites it — per-field
// last-writer-wins. updated_at is bumped to NOW() on every write.
const queryUpsertControlState = `
INSERT INTO go_vehicle_control_state
    (vehicle_id, is_locked, frunk_open, trunk_open, is_climate_on, charge_port_open, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (vehicle_id) DO UPDATE SET
    is_locked        = COALESCE(EXCLUDED.is_locked, go_vehicle_control_state.is_locked),
    frunk_open       = COALESCE(EXCLUDED.frunk_open, go_vehicle_control_state.frunk_open),
    trunk_open       = COALESCE(EXCLUDED.trunk_open, go_vehicle_control_state.trunk_open),
    is_climate_on    = COALESCE(EXCLUDED.is_climate_on, go_vehicle_control_state.is_climate_on),
    charge_port_open = COALESCE(EXCLUDED.charge_port_open, go_vehicle_control_state.charge_port_open),
    updated_at       = NOW()`

// UpsertControlState persists the present owner-control fields for the vehicle
// with the given cuid into the Go-owned go_vehicle_control_state side table.
// Absent (nil) fields are left untouched (per-field last-writer-wins via the
// COALESCE upsert). A no-field update is a no-op. This table has no Prisma FK
// (CG-DL-9), so a vehicle_id with no matching Prisma row simply stores an
// orphan control row that the snapshot left-join will never read — harmless.
func (r *VehicleRepo) UpsertControlState(ctx context.Context, vehicleID string, update ControlStateUpdate) error {
	if !update.HasAny() {
		return nil
	}
	start := time.Now()
	_, err := r.pool.Exec(ctx, queryUpsertControlState,
		vehicleID,
		update.IsLocked,
		update.FrunkOpen,
		update.TrunkOpen,
		update.IsClimateOn,
		update.ChargePortOpen,
	)
	r.metrics.ObserveQueryDuration("vehicle.upsert_control_state", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("vehicle.upsert_control_state")
		return fmt.Errorf("VehicleRepo.UpsertControlState(%s): %w", vehicleID, err)
	}
	return nil
}
