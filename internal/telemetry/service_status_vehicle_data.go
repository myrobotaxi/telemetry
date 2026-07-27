package telemetry

import (
	"context"
	"log/slog"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// notStreaming reports whether a vehicle is known NOT to be pushing live
// telemetry to us, and therefore needs a REST vehicle_data backfill for its
// stream-only owner-control fields (MYR-260). True when the car is in service
// (owner telemetry is paused / the car sits at a service center) OR Tesla's
// connectivity state is anything other than "online" (asleep / offline). A
// plain online, not-in-service car is streaming, so this returns false and no
// /vehicle_data call fires — the live stream already carries honest values.
//
// This is Tesla's LAGGING view: a car that is actively streaming to us can
// still read `asleep`/`offline` here for minutes. That is why it is not the
// only guard — refreshFromVehicleData additionally applies the MYR-300
// per-VIN stream-recency gate, using what we have actually received rather
// than what Tesla claims.
func notStreaming(state *FleetVehicleState) bool {
	return state.InService || !strings.EqualFold(state.State, "online")
}

// refreshFromVehicleData issues ONE Fleet API /vehicle_data read for a
// non-streaming vehicle and republishes the control-field subset as a
// synthetic VehicleTelemetryEvent so the values flow through the identical
// broadcast + persist path the live stream uses (WS mask / atomic-group
// projection, and store persistence for the columns that already exist).
//
// It NEVER force-wakes: any error (408 asleep, offline, decode failure) is a
// non-fatal skip that leaves the last-known state — iOS then renders an honest
// "unavailable" rather than a perpetual spinner. The error string is safe to
// log (redacted VIN + status; the P1 payload body is never surfaced).
//
// MYR-300 stream-recency gate. Tesla's connectivity flag lags reality, so this
// backfill also fires for cars that ARE streaming — and Tesla's /vehicle_data
// can answer from a stale cache. Because the synthetic frame carries
// CreatedAt = now, a non-nil stale value wins the COALESCE control-state
// upsert and durably overwrites fresher streamed state (the client-verified
// symptom: car screen "Cooling Down 69", app "Climate off"). So when a live
// streamed frame has arrived for this VIN inside the freshness window, every
// stream-sourceable field is dropped from the frame before publish — the
// stream is authoritative and current for all of them. REST-only fields
// (`trim`) still flow, and a genuinely non-streaming car (asleep, offline, in
// service) is unaffected: it has no fresh stamp, so the full backfill applies.
func (m *ServiceStatusMonitor) refreshFromVehicleData(ctx context.Context, vin, token string) {
	data, err := m.reader.GetVehicleData(ctx, token, vin)
	if err != nil {
		m.logger.Debug("service-status: vehicle_data backfill skipped (non-fatal, no wake)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return
	}

	fields := vehicleDataToFields(data)
	if m.streamFresh(vin) {
		dropped := dropStreamSourcedFields(fields)
		m.logger.Info("service-status: vehicle_data backfill gated by fresh stream (MYR-300)",
			slog.String("vin", redactVIN(vin)),
			slog.Int("dropped_fields", dropped),
			slog.Int("kept_fields", len(fields)),
			slog.Duration("freshness_window", m.freshness))
	}
	if len(fields) == 0 {
		return
	}
	if m.bus == nil {
		return // direct-handler unit tests construct the monitor without a bus
	}

	evt := events.VehicleTelemetryEvent{
		VIN:       vin,
		CreatedAt: m.now(), // becomes the wire `lastUpdated` → the app's "last synced"
		Fields:    fields,
		// Marks the frame as REST-sourced so the freshness gate above never
		// counts our own output as evidence the car is streaming (MYR-300).
		Source: events.SourceRESTBackfill,
	}
	if err := m.bus.Publish(ctx, events.NewEvent(evt)); err != nil {
		m.logger.Warn("service-status: vehicle_data broadcast publish failed (non-fatal)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
		return
	}
	m.logger.Info("service-status: populated owner controls from REST vehicle_data",
		slog.String("vin", redactVIN(vin)), slog.Int("fields", len(fields)))
}

// vehicleDataToFields maps the decoded REST vehicle_data subset onto internal
// telemetry field names — the SAME names the protobuf decoder emits — so the
// downstream broadcast (field_mapping.go) and persist (field_mapper.go) paths
// treat a REST-sourced frame identically to a streamed one.
//
// Mapping notes:
//   - Trunk positions (ft/rt ints) are folded back into the DoorState bitmask
//     so they flow through the identical splitDoorStateField -> frunkOpen /
//     trunkOpen unpack path used by the live stream.
//   - is_climate_on (bool) becomes hvacPower "On"/"Off"; field_mapping.go then
//     derives isClimateOn from it, matching the stream's behavior.
//   - inside/outside temps arrive in Celsius and are converted to Fahrenheit
//     (celsiusToFahrenheit), matching the stream's boundary conversion.
//   - charging_state strings match the proto 179 DetailedChargeState enum, so
//     chargeState passes through unchanged.
func vehicleDataToFields(data *VehicleData) map[string]events.TelemetryValue {
	fields := make(map[string]events.TelemetryValue)
	if data == nil {
		return fields
	}
	addVehicleStateFields(fields, data.VehicleState)
	addClimateStateFields(fields, data.ClimateState)
	addChargeStateFields(fields, data.ChargeState)
	addVehicleConfigFields(fields, data.VehicleConfig)
	return fields
}

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

// addVehicleConfigFields maps the vehicle_config subset: the trim badge (MYR-279).
// trim is not a streamed telemetry field, so this REST read is its only source;
// it flows through the identical control-state persist path as the streamed
// fields and surfaces on the /snapshot as `trim`.
func addVehicleConfigFields(fields map[string]events.TelemetryValue, vc *VehicleDataVehicleConfig) {
	if vc == nil {
		return
	}
	if vc.TrimBadging != nil && *vc.TrimBadging != "" {
		trim := *vc.TrimBadging
		fields[string(FieldTrim)] = events.TelemetryValue{StringVal: &trim}
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
