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

	// MYR-274 climate-MODE read-backs backing the owner Auto/Cool/Heat segment.
	// HvacAutoMode is the wire enum's string form ("On"/"Override"); a streamed
	// "Unknown"/empty is treated as never-read (nil) so it never overwrites a
	// known mode with a fabricated one, mirroring how IsClimateOn omits an
	// "Unknown" hvacPower. HvacAcEnabled is whether the A/C is on (*bool, a real
	// false overwrites).
	HvacAutoMode  *string
	HvacAcEnabled *bool

	// MYR-298 seat-ventilation + media-playback read-backs — the last two
	// MYR-252 cabin fields that were live-WebSocket-only, so a client that
	// missed the live frame could never learn them. SeatVentEnabled is a plain
	// bool (a real false overwrites). MediaPlaybackStatus is the wire enum's
	// string form ("Stopped"/"Playing"/"Paused"); a streamed "Unknown"/empty is
	// treated as never-read (nil) so it never overwrites a known status with a
	// fabricated one, exactly as HvacAutoMode handles "Unknown".
	SeatVentEnabled     *bool
	MediaPlaybackStatus *string

	// MYR-303 media NOW-PLAYING block. The five free-text fields diverge
	// deliberately from MediaPlaybackStatus above on empty values: that field is
	// an ENUM whose "Unknown" member means "we could not read this", so an
	// empty/Unknown is dropped to nil. These are FREE TEXT, where an empty value
	// means the car is telling us the track ENDED — a real observation that MUST
	// overwrite a stale title, or the panel advertises a song that stopped
	// playing an hour ago. So a present-but-empty string is kept (persisted as
	// '' and therefore wins the COALESCE upsert); only an ABSENT field stays
	// nil. Read back, '' is "nothing playing" and NULL is "never observed".
	//
	// The numerics follow the ordinary rule: a real observation including zero
	// overwrites. MediaNowPlayingDurationMs may legitimately hold Tesla's
	// 18000000 (5h) radio sentinel — stored as-is; rendering it as "no duration"
	// is the client's job per vehicle-state.schema.json.
	MediaNowPlayingTitle    *string
	MediaNowPlayingArtist   *string
	MediaNowPlayingAlbum    *string
	MediaNowPlayingStation  *string
	MediaPlaybackSource     *string
	MediaNowPlayingDuration *int64
	MediaNowPlayingElapsed  *int64
	MediaVolumeMax          *float64

	// MYR-308 ventilated-seat CAPABILITY — a spec fact, not a runtime state
	// (contrast SeatVentEnabled above, which is the on/off of that equipment).
	// REST-only: sourced from vehicle_config.has_seat_cooling by the MYR-260
	// backfill, exactly like Trim. A real false is authoritative ("this car has
	// no cooled seats") and overwrites.
	SeatCoolingCapable *bool

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

	// MYR-320 vehicle-DETAIL read-backs, the same side table and the same
	// snapshot LEFT JOIN as the MYR-279 pair above.
	//
	// TrimLabel is the DISPLAY-SAFE label from
	// vehicle_config.performance_package ("Performance"); it sits ALONGSIDE
	// Trim, which stays the raw badge code ("p74d"). Neither replaces the other
	// and only TrimLabel is ever rendered.
	//
	// FSDVersion is the FSD software designation from the newest release-notes
	// TITLE ("FSD (Supervised) v14.3.5") — a different endpoint and a different
	// value from SoftwareVersion, which is the installed firmware build.
	//
	// Both follow the MYR-279 empty-string-drop rule rather than the MYR-303
	// media rule: these are facts about the car that only get better-known, so
	// an empty read is "we learned nothing", never "the value went away".
	TrimLabel  *string
	FSDVersion *string
}

// HasAny reports whether at least one control field is present. The writer
// skips the side-table upsert entirely when a telemetry frame carries none of
// the control fields, so an ordinary speed/location frame never touches the
// table.
func (c *ControlStateUpdate) HasAny() bool {
	if c == nil {
		return false
	}
	return c.hasBoolControl() || c.hasLevelControl() || c.hasStringControl() ||
		c.hasClimateMode() || c.hasSeatVentMedia() || c.hasMediaNowPlaying() ||
		c.SeatCoolingCapable != nil
}

// hasMediaNowPlaying reports whether any MYR-303 now-playing field is present.
// A present-but-EMPTY text field counts: an empty title is the car saying the
// track ended, which is a real observation worth an upsert.
func (c *ControlStateUpdate) hasMediaNowPlaying() bool {
	return c.MediaNowPlayingTitle != nil ||
		c.MediaNowPlayingArtist != nil ||
		c.MediaNowPlayingAlbum != nil ||
		c.MediaNowPlayingStation != nil ||
		c.MediaPlaybackSource != nil ||
		c.MediaNowPlayingDuration != nil ||
		c.MediaNowPlayingElapsed != nil ||
		c.MediaVolumeMax != nil
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

// hasStringControl reports whether any vehicle-detail string is present — the
// MYR-279 pair (software version, trim) plus the MYR-320 pair (trim label, FSD
// version).
func (c *ControlStateUpdate) hasStringControl() bool {
	return c.SoftwareVersion != nil || c.Trim != nil ||
		c.TrimLabel != nil || c.FSDVersion != nil
}

// hasClimateMode reports whether any MYR-274 climate-mode read-back (hvac auto
// mode, A/C enabled) is present.
func (c *ControlStateUpdate) hasClimateMode() bool {
	return c.HvacAutoMode != nil || c.HvacAcEnabled != nil
}

// hasSeatVentMedia reports whether any MYR-298 read-back (seat ventilation,
// media playback status) is present.
func (c *ControlStateUpdate) hasSeatVentMedia() bool {
	return c.SeatVentEnabled != nil || c.MediaPlaybackStatus != nil
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
	dst.TrimLabel = mergePtr(dst.TrimLabel, src.TrimLabel)
	dst.FSDVersion = mergePtr(dst.FSDVersion, src.FSDVersion)
	dst.HvacAutoMode = mergePtr(dst.HvacAutoMode, src.HvacAutoMode)
	dst.HvacAcEnabled = mergePtr(dst.HvacAcEnabled, src.HvacAcEnabled)
	dst.SeatVentEnabled = mergePtr(dst.SeatVentEnabled, src.SeatVentEnabled)
	dst.MediaPlaybackStatus = mergePtr(dst.MediaPlaybackStatus, src.MediaPlaybackStatus)
	mergeMediaNowPlaying(dst, src)
	dst.SeatCoolingCapable = mergePtr(dst.SeatCoolingCapable, src.SeatCoolingCapable)
}

// mergeMediaNowPlaying folds the MYR-303 now-playing fields of src onto dst.
// Split out of mergeControlState to keep that function under the funlen cap.
// mergePtr keeps a non-nil src value even when it is an empty string, which is
// what makes "the track ended" propagate through the writer's coalescing buffer
// instead of being swallowed before it ever reaches the upsert.
func mergeMediaNowPlaying(dst, src *ControlStateUpdate) {
	dst.MediaNowPlayingTitle = mergePtr(dst.MediaNowPlayingTitle, src.MediaNowPlayingTitle)
	dst.MediaNowPlayingArtist = mergePtr(dst.MediaNowPlayingArtist, src.MediaNowPlayingArtist)
	dst.MediaNowPlayingAlbum = mergePtr(dst.MediaNowPlayingAlbum, src.MediaNowPlayingAlbum)
	dst.MediaNowPlayingStation = mergePtr(dst.MediaNowPlayingStation, src.MediaNowPlayingStation)
	dst.MediaPlaybackSource = mergePtr(dst.MediaPlaybackSource, src.MediaPlaybackSource)
	dst.MediaNowPlayingDuration = mergePtr(dst.MediaNowPlayingDuration, src.MediaNowPlayingDuration)
	dst.MediaNowPlayingElapsed = mergePtr(dst.MediaNowPlayingElapsed, src.MediaNowPlayingElapsed)
	dst.MediaVolumeMax = mergePtr(dst.MediaVolumeMax, src.MediaVolumeMax)
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
	mapControlClimateMode(fields, c)
	mapControlSeatVentMedia(fields, c)
	mapControlMediaNowPlaying(fields, c)
	mapControlCapability(fields, c)
	if !c.HasAny() {
		return nil
	}
	return c
}

// mapControlClimateMode derives the MYR-274 climate-mode read-backs (hvac auto
// mode, A/C enabled) onto c. hvacAutoMode arrives as the enum's string form
// ("On"/"Override"/"Unknown"); an empty OR "Unknown" value is OMITTED (left nil)
// so a genuinely-unknown mode never overwrites a known value with a fabricated
// one — the same honest-unknown discipline climateOnFromHvacPower applies to
// "Unknown" hvacPower (MYR-251/252). hvacAcEnabled is a plain bool that persists
// when present, including a real false.
func mapControlClimateMode(fields map[string]events.TelemetryValue, c *ControlStateUpdate) {
	if v, ok := fields[string(telemetry.FieldHvacAutoMode)]; ok && !v.Invalid && v.StringVal != nil {
		if mode := *v.StringVal; mode != "" && !equalFoldASCII(mode, "Unknown") {
			c.HvacAutoMode = &mode
		}
	}

	if v, ok := fields[string(telemetry.FieldHvacACEnabled)]; ok && !v.Invalid && v.BoolVal != nil {
		enabled := *v.BoolVal
		c.HvacAcEnabled = &enabled
	}
}

// mapControlSeatVentMedia derives the MYR-298 read-backs (seat ventilation,
// media playback status) onto c. seatVentEnabled is a plain bool that persists
// when present, including a real false. mediaPlaybackStatus arrives as the enum's
// string form ("Stopped"/"Playing"/"Paused"/"Unknown"); an empty OR "Unknown"
// value is OMITTED (left nil) so a genuinely-unknown status never overwrites a
// known one with a fabricated value — the same honest-unknown discipline
// mapControlClimateMode applies to "Unknown" hvacAutoMode (MYR-251/252/274).
//
// Neither field is emitted by the MYR-260 /vehicle_data backfill
// (vehicleDataToFields) — Tesla's cached vehicle_data climate subset carries
// neither — so this mapping only ever fires for a live streamed frame.
func mapControlSeatVentMedia(fields map[string]events.TelemetryValue, c *ControlStateUpdate) {
	if v, ok := fields[string(telemetry.FieldSeatVentEnabled)]; ok && !v.Invalid && v.BoolVal != nil {
		enabled := *v.BoolVal
		c.SeatVentEnabled = &enabled
	}

	if v, ok := fields[string(telemetry.FieldMediaPlaybackStatus)]; ok && !v.Invalid && v.StringVal != nil {
		if status := *v.StringVal; status != "" && !equalFoldASCII(status, "Unknown") {
			c.MediaPlaybackStatus = &status
		}
	}
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

// mapControlStrings derives the vehicle-detail read-backs onto c: the MYR-279
// pair (software version, trim) and the MYR-320 pair (trim label, FSD version).
// All four are plain strings — software version from the Version telemetry field
// (streamed OR the /vehicle_data car_version), trim and trim label from
// /vehicle_data's vehicle_config, FSD version from the release-notes title —
// and all four share one rule, so they are table-driven rather than repeated.
//
// Empty strings are IGNORED so a blank frame never overwrites a known value with
// "". That is the deliberate divergence from the MYR-303 media text fields,
// where an empty value is a real observation ("the track ended") that must
// overwrite: these four are facts about the car that only ever become
// better-known, so an empty read means "we learned nothing", never "the value
// went away".
func mapControlStrings(fields map[string]events.TelemetryValue, c *ControlStateUpdate) {
	for field, target := range controlStringFields {
		v, ok := fields[string(field)]
		if !ok || v.Invalid || v.StringVal == nil || *v.StringVal == "" {
			continue
		}
		s := *v.StringVal
		*target(c) = &s
	}
}

// controlStringFields maps each vehicle-detail telemetry field name onto the
// ControlStateUpdate pointer it fills. Table-driven so a fifth detail string
// costs one line here instead of a fourth copy of the same guard.
var controlStringFields = map[telemetry.FieldName]func(*ControlStateUpdate) **string{
	telemetry.FieldVersion:    func(c *ControlStateUpdate) **string { return &c.SoftwareVersion },
	telemetry.FieldTrim:       func(c *ControlStateUpdate) **string { return &c.Trim },
	telemetry.FieldTrimLabel:  func(c *ControlStateUpdate) **string { return &c.TrimLabel },
	telemetry.FieldFSDVersion: func(c *ControlStateUpdate) **string { return &c.FSDVersion },
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
     software_version, trim, hvac_auto_mode, hvac_ac_enabled,
     seat_vent_enabled, media_playback_status,
     media_now_playing_title, media_now_playing_artist, media_now_playing_album,
     media_now_playing_station, media_playback_source,
     media_now_playing_duration_ms, media_now_playing_elapsed_ms, media_volume_max,
     seat_cooling_capable, trim_label, fsd_version, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21,
        $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, NOW())
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
    hvac_auto_mode          = COALESCE(EXCLUDED.hvac_auto_mode, go_vehicle_control_state.hvac_auto_mode),
    hvac_ac_enabled         = COALESCE(EXCLUDED.hvac_ac_enabled, go_vehicle_control_state.hvac_ac_enabled),
    seat_vent_enabled       = COALESCE(EXCLUDED.seat_vent_enabled, go_vehicle_control_state.seat_vent_enabled),
    media_playback_status   = COALESCE(EXCLUDED.media_playback_status, go_vehicle_control_state.media_playback_status),
    media_now_playing_title       = COALESCE(EXCLUDED.media_now_playing_title, go_vehicle_control_state.media_now_playing_title),
    media_now_playing_artist      = COALESCE(EXCLUDED.media_now_playing_artist, go_vehicle_control_state.media_now_playing_artist),
    media_now_playing_album       = COALESCE(EXCLUDED.media_now_playing_album, go_vehicle_control_state.media_now_playing_album),
    media_now_playing_station     = COALESCE(EXCLUDED.media_now_playing_station, go_vehicle_control_state.media_now_playing_station),
    media_playback_source         = COALESCE(EXCLUDED.media_playback_source, go_vehicle_control_state.media_playback_source),
    media_now_playing_duration_ms = COALESCE(EXCLUDED.media_now_playing_duration_ms, go_vehicle_control_state.media_now_playing_duration_ms),
    media_now_playing_elapsed_ms  = COALESCE(EXCLUDED.media_now_playing_elapsed_ms, go_vehicle_control_state.media_now_playing_elapsed_ms),
    media_volume_max              = COALESCE(EXCLUDED.media_volume_max, go_vehicle_control_state.media_volume_max),
    seat_cooling_capable          = COALESCE(EXCLUDED.seat_cooling_capable, go_vehicle_control_state.seat_cooling_capable),
    trim_label                    = COALESCE(EXCLUDED.trim_label, go_vehicle_control_state.trim_label),
    fsd_version                   = COALESCE(EXCLUDED.fsd_version, go_vehicle_control_state.fsd_version),
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
		update.HvacAutoMode,
		update.HvacAcEnabled,
		update.SeatVentEnabled,
		update.MediaPlaybackStatus,
		// MYR-303: a non-nil EMPTY string binds as '' (not NULL), so it wins the
		// COALESCE above and clears a stale track — the deliberate divergence
		// from media_playback_status two lines up. See the ControlStateUpdate
		// field comments.
		update.MediaNowPlayingTitle,
		update.MediaNowPlayingArtist,
		update.MediaNowPlayingAlbum,
		update.MediaNowPlayingStation,
		update.MediaPlaybackSource,
		update.MediaNowPlayingDuration,
		update.MediaNowPlayingElapsed,
		update.MediaVolumeMax,
		update.SeatCoolingCapable,
		// MYR-320: empty strings never reach here — mapControlStrings drops
		// them — so a NULL bind always means "this frame said nothing" and the
		// COALESCE correctly keeps the stored value.
		update.TrimLabel,
		update.FSDVersion,
	)
	r.metrics.ObserveQueryDuration("vehicle.upsert_control_state", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("vehicle.upsert_control_state")
		return fmt.Errorf("VehicleRepo.UpsertControlState(%s): %w", vehicleID, err)
	}
	return nil
}
