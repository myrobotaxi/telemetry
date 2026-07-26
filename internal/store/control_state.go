package store

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// ControlStateUpdate holds the owner-control read-back values persisted to the
// Go-owned go_vehicle_control_state side table. MYR-269 added the five owner
// controls the app renders as toggles — lock, trunk + frunk, climate, and
// charge-port state (all *bool). MYR-273 adds the cabin SETTING levels the owner
// sheet renders as numbers — the driver/passenger temperature setpoints, fan
// speed, the front/rear seat heater and front seat cooler levels (all *int), and
// the media volume (*float64).
//
// Every field is a pointer: a nil pointer means "this frame did not carry the
// value", so the upsert leaves the stored column untouched (last-writer-wins PER
// FIELD). A non-nil value is a real observation and DOES overwrite (including a
// bool false or an int/float zero).
//
// These are the same MYR-252 cabin read-backs the live WebSocket stream carries
// as `locked`, `frunkOpen`/`trunkOpen`, `isClimateOn`, `chargePortDoorOpen`,
// `driverTempSetting`, `passengerTempSetting`, `fanSpeed`, `seatHeater*`,
// `seatCooler*`, and `mediaVolume`; MYR-269 + MYR-273 make them durable so a
// /snapshot for a non-streaming car returns the last-known value instead of a
// perpetual "—".
type ControlStateUpdate struct {
	IsLocked       *bool
	FrunkOpen      *bool
	TrunkOpen      *bool
	IsClimateOn    *bool
	ChargePortOpen *bool

	// MYR-273 cabin-setting levels. Temp setpoints are Fahrenheit-rounded ints
	// (converted Celsius→Fahrenheit at the telemetry boundary, like interiorTemp);
	// fan speed and seat heater/cooler levels are small non-negative ints; media
	// volume is a fractional level (typically 0-11) so it is a *float64.
	DriverTempSetting    *int
	PassengerTempSetting *int
	FanSpeed             *int
	SeatHeaterLeft       *int
	SeatHeaterRight      *int
	SeatHeaterRearLeft   *int
	SeatHeaterRearCenter *int
	SeatHeaterRearRight  *int
	SeatCoolerLeft       *int
	SeatCoolerRight      *int
	MediaVolume          *float64

	// MYR-279 vehicle-DETAIL read-backs. Not cabin controls, but the same
	// Go-owned side table + snapshot LEFT JOIN carry them (they have no home
	// in the Prisma-owned "Vehicle" table). Both nullable strings: software
	// version streams (Tesla proto Version) OR arrives via the MYR-260
	// /vehicle_data backfill (car_version); trim arrives ONLY via the
	// /vehicle_data backfill (vehicle_config.trim_badging -- Tesla does not
	// stream it).
	SoftwareVersion *string
	Trim            *string
}

// HasAny reports whether at least one control field is present. The writer
// skips the side-table upsert entirely when a telemetry frame carries none of
// the control fields, so an ordinary speed/location frame never touches the
// table.
func (c *ControlStateUpdate) HasAny() bool {
	if c == nil {
		return false
	}
	return c.hasBoolControl() || c.hasLevelControl() || c.hasStringControl()
}

// hasBoolControl reports whether any MYR-269 owner-control boolean is present.
// Split out of HasAny to keep each helper under the cyclop complexity cap.
func (c *ControlStateUpdate) hasBoolControl() bool {
	return c.IsLocked != nil ||
		c.FrunkOpen != nil ||
		c.TrunkOpen != nil ||
		c.IsClimateOn != nil ||
		c.ChargePortOpen != nil
}

// hasLevelControl reports whether any MYR-273 cabin-setting level is present.
func (c *ControlStateUpdate) hasLevelControl() bool {
	return c.DriverTempSetting != nil ||
		c.PassengerTempSetting != nil ||
		c.FanSpeed != nil ||
		c.SeatHeaterLeft != nil ||
		c.SeatHeaterRight != nil ||
		c.SeatHeaterRearLeft != nil ||
		c.SeatHeaterRearCenter != nil ||
		c.SeatHeaterRearRight != nil ||
		c.SeatCoolerLeft != nil ||
		c.SeatCoolerRight != nil ||
		c.MediaVolume != nil
}

// hasStringControl reports whether any MYR-279 vehicle-detail string (software
// version, trim) is present.
func (c *ControlStateUpdate) hasStringControl() bool {
	return c.SoftwareVersion != nil || c.Trim != nil
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
	dst.DriverTempSetting = mergePtr(dst.DriverTempSetting, src.DriverTempSetting)
	dst.PassengerTempSetting = mergePtr(dst.PassengerTempSetting, src.PassengerTempSetting)
	dst.FanSpeed = mergePtr(dst.FanSpeed, src.FanSpeed)
	dst.SeatHeaterLeft = mergePtr(dst.SeatHeaterLeft, src.SeatHeaterLeft)
	dst.SeatHeaterRight = mergePtr(dst.SeatHeaterRight, src.SeatHeaterRight)
	dst.SeatHeaterRearLeft = mergePtr(dst.SeatHeaterRearLeft, src.SeatHeaterRearLeft)
	dst.SeatHeaterRearCenter = mergePtr(dst.SeatHeaterRearCenter, src.SeatHeaterRearCenter)
	dst.SeatHeaterRearRight = mergePtr(dst.SeatHeaterRearRight, src.SeatHeaterRearRight)
	dst.SeatCoolerLeft = mergePtr(dst.SeatCoolerLeft, src.SeatCoolerLeft)
	dst.SeatCoolerRight = mergePtr(dst.SeatCoolerRight, src.SeatCoolerRight)
	dst.MediaVolume = mergePtr(dst.MediaVolume, src.MediaVolume)
	dst.SoftwareVersion = mergePtr(dst.SoftwareVersion, src.SoftwareVersion)
	dst.Trim = mergePtr(dst.Trim, src.Trim)
}

// controlIntFields maps each cabin-setting telemetry field that persists as a
// nullable INT to the pointer slot it fills on a ControlStateUpdate. Keeping the
// mapping table-driven keeps mapTelemetryToControlState flat (cyclop) — adding a
// cabin level is a one-line entry, not another if-block. Uses the SAME internal
// field names the protobuf decoder and the MYR-260 /vehicle_data backfill emit
// (internal/telemetry/fields.go). Note: FieldHvacFanSpeed's internal name is
// "hvacFanSpeed"; the WS layer translates it to the wire name "fanSpeed", which
// is also the column name here (fan_speed).
var controlIntFields = map[telemetry.FieldName]func(*ControlStateUpdate) **int{
	telemetry.FieldDriverTempSetting:    func(c *ControlStateUpdate) **int { return &c.DriverTempSetting },
	telemetry.FieldPassengerTempSetting: func(c *ControlStateUpdate) **int { return &c.PassengerTempSetting },
	telemetry.FieldHvacFanSpeed:         func(c *ControlStateUpdate) **int { return &c.FanSpeed },
	telemetry.FieldSeatHeaterLeft:       func(c *ControlStateUpdate) **int { return &c.SeatHeaterLeft },
	telemetry.FieldSeatHeaterRight:      func(c *ControlStateUpdate) **int { return &c.SeatHeaterRight },
	telemetry.FieldSeatHeaterRearLeft:   func(c *ControlStateUpdate) **int { return &c.SeatHeaterRearLeft },
	telemetry.FieldSeatHeaterRearCenter: func(c *ControlStateUpdate) **int { return &c.SeatHeaterRearCenter },
	telemetry.FieldSeatHeaterRearRight:  func(c *ControlStateUpdate) **int { return &c.SeatHeaterRearRight },
	telemetry.FieldSeatCoolerLeft:       func(c *ControlStateUpdate) **int { return &c.SeatCoolerLeft },
	telemetry.FieldSeatCoolerRight:      func(c *ControlStateUpdate) **int { return &c.SeatCoolerRight },
}

// mapTelemetryToControlState derives the owner-control read-backs from a
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
//   - driver/passengerTempSetting, hvacFanSpeed, seatHeater*, seatCooler* (int) →
//     the matching *int level (MYR-273). Numeric fields arrive as either IntVal or
//     FloatVal depending on firmware; a float is rounded to nearest int, matching
//     the WS layer's roundIfInteger.
//   - mediaVolume (number)       → MediaVolume (*float64, NOT rounded — fractional)
//
// Fields marked Invalid by the vehicle are ignored (these controls are not
// atomic-group / clear-on-invalid fields). Returns nil when no control field is
// present so callers can cheaply skip the side-table write.
func mapTelemetryToControlState(fields map[string]events.TelemetryValue) *ControlStateUpdate {
	c := &ControlStateUpdate{}
	mapControlBooleans(fields, c)
	mapControlLevels(fields, c)
	mapControlStrings(fields, c)
	if !c.HasAny() {
		return nil
	}
	return c
}

// mapControlBooleans derives the MYR-269 owner-control booleans (lock, frunk/
// trunk, climate, charge-port) onto c. Split from mapTelemetryToControlState to
// keep each derivation helper under the cognitive-complexity cap.
func mapControlBooleans(fields map[string]events.TelemetryValue, c *ControlStateUpdate) {
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
}

// mapControlLevels derives the MYR-273 cabin-setting levels (temp setpoints, fan
// speed, seat heater/cooler levels, media volume) onto c. The integer levels are
// table-driven via controlIntFields; media volume stays fractional (not rounded).
func mapControlLevels(fields map[string]events.TelemetryValue, c *ControlStateUpdate) {
	for field, target := range controlIntFields {
		if v, ok := fields[string(field)]; ok && !v.Invalid {
			if iv := controlIntFromValue(v); iv != nil {
				*target(c) = iv
			}
		}
	}

	if v, ok := fields[string(telemetry.FieldMediaVolume)]; ok && !v.Invalid {
		if fv := controlFloatFromValue(v); fv != nil {
			c.MediaVolume = fv
		}
	}
}

// mapControlStrings derives the MYR-279 vehicle-detail read-backs (software
// version, trim) onto c. Both are plain strings: software version from the
// Version telemetry field (streamed OR the /vehicle_data car_version), trim from
// the /vehicle_data-only trim field. Empty strings are ignored so a blank frame
// never overwrites a known value with "".
func mapControlStrings(fields map[string]events.TelemetryValue, c *ControlStateUpdate) {
	if v, ok := fields[string(telemetry.FieldVersion)]; ok && !v.Invalid && v.StringVal != nil && *v.StringVal != "" {
		s := *v.StringVal
		c.SoftwareVersion = &s
	}
	if v, ok := fields[string(telemetry.FieldTrim)]; ok && !v.Invalid && v.StringVal != nil && *v.StringVal != "" {
		s := *v.StringVal
		c.Trim = &s
	}
}

// controlIntFromValue extracts a nullable int level from a TelemetryValue. The
// numeric cabin fields arrive as IntVal on newer firmware and FloatVal (or a
// numeric string parsed to FloatVal) on older firmware; a float is rounded to
// the nearest int, matching the WS layer's roundIfInteger so the persisted value
// equals the live wire value. Returns nil when the value carries no number.
func controlIntFromValue(v events.TelemetryValue) *int {
	switch {
	case v.IntVal != nil:
		i := int(*v.IntVal)
		return &i
	case v.FloatVal != nil:
		i := int(math.Round(*v.FloatVal))
		return &i
	default:
		return nil
	}
}

// controlFloatFromValue extracts a nullable float level from a TelemetryValue,
// used for mediaVolume (a fractional level the WS layer intentionally does NOT
// round). An IntVal is widened to float64 for the rare firmware that sends the
// volume as an integer. Returns nil when the value carries no number.
func controlFloatFromValue(v events.TelemetryValue) *float64 {
	switch {
	case v.FloatVal != nil:
		f := *v.FloatVal
		return &f
	case v.IntVal != nil:
		f := float64(*v.IntVal)
		return &f
	default:
		return nil
	}
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
// (a real observation, including false / 0) overwrites it — per-field
// last-writer-wins. updated_at is bumped to NOW() on every write.
const queryUpsertControlState = `
INSERT INTO go_vehicle_control_state
    (vehicle_id, is_locked, frunk_open, trunk_open, is_climate_on, charge_port_open,
     driver_temp_setting, passenger_temp_setting, fan_speed,
     seat_heater_left, seat_heater_right,
     seat_heater_rear_left, seat_heater_rear_center, seat_heater_rear_right,
     seat_cooler_left, seat_cooler_right, media_volume,
     software_version, trim, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, NOW())
ON CONFLICT (vehicle_id) DO UPDATE SET
    is_locked               = COALESCE(EXCLUDED.is_locked, go_vehicle_control_state.is_locked),
    frunk_open              = COALESCE(EXCLUDED.frunk_open, go_vehicle_control_state.frunk_open),
    trunk_open              = COALESCE(EXCLUDED.trunk_open, go_vehicle_control_state.trunk_open),
    is_climate_on           = COALESCE(EXCLUDED.is_climate_on, go_vehicle_control_state.is_climate_on),
    charge_port_open        = COALESCE(EXCLUDED.charge_port_open, go_vehicle_control_state.charge_port_open),
    driver_temp_setting     = COALESCE(EXCLUDED.driver_temp_setting, go_vehicle_control_state.driver_temp_setting),
    passenger_temp_setting  = COALESCE(EXCLUDED.passenger_temp_setting, go_vehicle_control_state.passenger_temp_setting),
    fan_speed               = COALESCE(EXCLUDED.fan_speed, go_vehicle_control_state.fan_speed),
    seat_heater_left        = COALESCE(EXCLUDED.seat_heater_left, go_vehicle_control_state.seat_heater_left),
    seat_heater_right       = COALESCE(EXCLUDED.seat_heater_right, go_vehicle_control_state.seat_heater_right),
    seat_heater_rear_left   = COALESCE(EXCLUDED.seat_heater_rear_left, go_vehicle_control_state.seat_heater_rear_left),
    seat_heater_rear_center = COALESCE(EXCLUDED.seat_heater_rear_center, go_vehicle_control_state.seat_heater_rear_center),
    seat_heater_rear_right  = COALESCE(EXCLUDED.seat_heater_rear_right, go_vehicle_control_state.seat_heater_rear_right),
    seat_cooler_left        = COALESCE(EXCLUDED.seat_cooler_left, go_vehicle_control_state.seat_cooler_left),
    seat_cooler_right       = COALESCE(EXCLUDED.seat_cooler_right, go_vehicle_control_state.seat_cooler_right),
    media_volume            = COALESCE(EXCLUDED.media_volume, go_vehicle_control_state.media_volume),
    software_version        = COALESCE(EXCLUDED.software_version, go_vehicle_control_state.software_version),
    trim                    = COALESCE(EXCLUDED.trim, go_vehicle_control_state.trim),
    updated_at              = NOW()`

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
		update.DriverTempSetting,
		update.PassengerTempSetting,
		update.FanSpeed,
		update.SeatHeaterLeft,
		update.SeatHeaterRight,
		update.SeatHeaterRearLeft,
		update.SeatHeaterRearCenter,
		update.SeatHeaterRearRight,
		update.SeatCoolerLeft,
		update.SeatCoolerRight,
		update.MediaVolume,
		update.SoftwareVersion,
		update.Trim,
	)
	r.metrics.ObserveQueryDuration("vehicle.upsert_control_state", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("vehicle.upsert_control_state")
		return fmt.Errorf("VehicleRepo.UpsertControlState(%s): %w", vehicleID, err)
	}
	return nil
}
