package telemetry

// Per-sub-object mappers for the MYR-260 REST /vehicle_data backfill. Split out
// of service_status_vehicle_data.go (which owns the read, the MYR-300 freshness
// gate and the publish) so that file stays inside the 300-line cap as the
// MYR-320 detail fields grow the vehicle_config mapper alongside it.
//
// Every mapper writes the SAME internal field names the protobuf decoder emits,
// so the downstream broadcast (field_mapping.go) and persist (field_mapper.go)
// paths treat a REST-sourced frame identically to a streamed one.

import (
	"github.com/myrobotaxi/telemetry/internal/events"
)

// addVehicleStateFields maps the vehicle_state subset: lock, trunk positions
// (folded into the DoorState bitmask so the frunk/trunk unpack path is reused),
// and odometer.
func addVehicleStateFields(fields map[string]events.TelemetryValue, vs *VehicleDataVehicleState) {
	if vs == nil {
		return
	}
	if vs.Locked != nil {
		fields[string(FieldLocked)] = events.TelemetryValue{BoolVal: vs.Locked}
	}
	if vs.FrontTrunk != nil || vs.RearTrunk != nil {
		bits := doorBitsFromTrunks(vs.FrontTrunk, vs.RearTrunk)
		fields[string(FieldDoorState)] = events.TelemetryValue{IntVal: &bits}
	}
	if vs.Odometer != nil {
		o := *vs.Odometer
		fields[string(FieldOdometer)] = events.TelemetryValue{FloatVal: &o}
	}
	if vs.CarVersion != nil && *vs.CarVersion != "" {
		ver := *vs.CarVersion
		fields[string(FieldVersion)] = events.TelemetryValue{StringVal: &ver}
	}
}

// addVehicleConfigFields maps the vehicle_config subset: the trim badge
// (MYR-279) and the ventilated-seat capability (MYR-308). Neither is a streamed
// telemetry field, so this REST read is their only source; both flow through the
// identical control-state persist path as the streamed fields and surface on the
// /snapshot as `trim` and `seatCoolingCapable`.
//
// Being REST-ONLY is precisely what carries them past the MYR-300 stream-recency
// gate. That gate drops every field in streamSourcedFields, which is built from
// fieldMap (service_status_stream_freshness.go) — and neither FieldTrim nor
// FieldSeatCoolingCapable is in fieldMap, because Tesla has no proto for either.
// So a car that is busily streaming still acquires them, and an IN-SERVICE car —
// which never streams at all, and is the case MYR-308 exists for — acquires the
// capability on the ordinary connectivity-edge read with no stream required.
//
// vehicle_config needs no `endpoints=` query parameter: GetVehicleData calls
// /vehicle_data bare, and Tesla's default response already includes the
// vehicle_config sub-object (that is how MYR-279's trim_badging arrives today).
//
// A real FALSE for has_seat_cooling is KEPT, unlike the empty-string skip on
// trim above: false is the authoritative "this car has no cooled seats", which
// is the whole value of the field — it lets a client stop offering a control the
// hardware cannot honour. Only an ABSENT key leaves the column NULL.
func addVehicleConfigFields(fields map[string]events.TelemetryValue, vc *VehicleDataVehicleConfig) {
	if vc == nil {
		return
	}
	if vc.TrimBadging != nil && *vc.TrimBadging != "" {
		trim := *vc.TrimBadging
		fields[string(FieldTrim)] = events.TelemetryValue{StringVal: &trim}
	}
	// MYR-320: performance_package is the DISPLAY-SAFE label, kept ALONGSIDE the
	// raw badge above rather than replacing it. Same empty-string skip as trim: a
	// blank label must never overwrite a known one.
	if vc.PerformancePackage != nil && *vc.PerformancePackage != "" {
		label := *vc.PerformancePackage
		fields[string(FieldTrimLabel)] = events.TelemetryValue{StringVal: &label}
	}
	// exterior_color is deliberately NOT mapped here. It is the one
	// vehicle_config value with an EXISTING wire field (`color`) fed by an
	// EXISTING column (Prisma "Vehicle".color), so it takes the narrow
	// column-write path (syncVehicleColor) instead of travelling as a telemetry
	// field. Routing it through here would need a new wire field for a value the
	// contract already ships.
	if vc.HasSeatCooling != nil {
		capable := *vc.HasSeatCooling
		fields[string(FieldSeatCoolingCapable)] = events.TelemetryValue{BoolVal: &capable}
	}
}

// doorBitsFromTrunks folds the ft/rt open/closed ints back into the DoorState
// bitmask layout shared with the stream decoder (0 = closed, non-zero = open).
func doorBitsFromTrunks(front, rear *int) int64 {
	var bits int64
	if front != nil && *front != 0 {
		bits |= int64(events.DoorFrunk)
	}
	if rear != nil && *rear != 0 {
		bits |= int64(events.DoorTrunk)
	}
	return bits
}

// addClimateStateFields maps the climate_state subset: is_climate_on (as the
// hvacPower enum string) and cabin/ambient temps (Celsius -> Fahrenheit).
func addClimateStateFields(fields map[string]events.TelemetryValue, cs *VehicleDataClimateState) {
	if cs == nil {
		return
	}
	if cs.IsClimateOn != nil {
		p := hvacPowerFromBool(*cs.IsClimateOn)
		fields[string(FieldHvacPower)] = events.TelemetryValue{StringVal: &p}
	}
	if cs.InsideTemp != nil {
		f := celsiusToFahrenheit(*cs.InsideTemp)
		fields[string(FieldInsideTemp)] = events.TelemetryValue{FloatVal: &f}
	}
	if cs.OutsideTemp != nil {
		f := celsiusToFahrenheit(*cs.OutsideTemp)
		fields[string(FieldOutsideTemp)] = events.TelemetryValue{FloatVal: &f}
	}
}

// addChargeStateFields maps the charge_state subset: charging_state (proto 179
// DetailedChargeState enum strings), charge-port door, and battery level.
func addChargeStateFields(fields map[string]events.TelemetryValue, ch *VehicleDataChargeState) {
	if ch == nil {
		return
	}
	if ch.ChargingState != nil {
		fields[string(FieldChargeState)] = events.TelemetryValue{StringVal: ch.ChargingState}
	}
	if ch.ChargePortDoorOpen != nil {
		fields[string(FieldChargePortDoorOpen)] = events.TelemetryValue{BoolVal: ch.ChargePortDoorOpen}
	}
	if ch.BatteryLevel != nil {
		// Emit under FieldSOC ("soc"), NOT FieldBatteryLevel: only "soc"
		// translates to the wire "chargeLevel" (field_mapping.go) and is on the
		// owner mask allow-list. "batteryLevel" is dropped by mask.Apply, so the
		// charge % would never reach the app over live WS (MYR-260 review). Tesla
		// REST battery_level is the same usable SOC %.
		bl := float64(*ch.BatteryLevel)
		fields[string(FieldSOC)] = events.TelemetryValue{FloatVal: &bl}
	}
}

// hvacPowerFromBool renders is_climate_on as the capitalized hvacPower enum
// string the stream emits, so field_mapping.go derives isClimateOn identically.
func hvacPowerFromBool(on bool) string {
	if on {
		return "On"
	}
	return "Off"
}
