package telemetry

import (
	"context"
	"errors"
	"fmt"
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
	err := m.RefreshFromVehicleData(ctx, vin, token)
	switch {
	case err == nil:
		return
	case errors.Is(err, ErrVehicleDataPublish):
		// A bus failure is ours, not the car's, and is rare enough to be worth
		// operator attention.
		m.logger.Warn("service-status: vehicle_data broadcast publish failed (non-fatal)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
	default:
		// A failed read is the ordinary outcome for a sleeping car (Tesla answers
		// 408), so it stays at Debug — promoting it would spam the logs for every
		// parked car in the fleet.
		m.logger.Debug("service-status: vehicle_data backfill skipped (non-fatal, no wake)",
			slog.String("vin", redactVIN(vin)), slog.String("error", err.Error()))
	}
}

// ErrVehicleDataRead / ErrVehicleDataPublish classify a RefreshFromVehicleData
// failure without exposing the underlying Fleet API error type. The read
// sentinel covers "the car did not answer" (408 asleep, offline, decode
// failure); the publish sentinel covers "we could not fan the frame out".
// Callers use errors.Is — the event-path backfill to pick a log level, and the
// MYR-315 refresh handler to decide whether a 502 or a 500 is honest.
var (
	ErrVehicleDataRead    = errors.New("vehicle_data read failed")
	ErrVehicleDataPublish = errors.New("vehicle_data broadcast publish failed")
)

// RefreshFromVehicleData is the error-returning core of the vehicle_data
// backfill: ONE Fleet API read, mapped through vehicleDataToFields, gated by
// the MYR-300 stream-recency rule, and republished as a synthetic
// REST-backfill VehicleTelemetryEvent.
//
// Exported so the MYR-315 owner-triggered refresh endpoint publishes through
// the IDENTICAL MYR-260 mapping the connectivity-edge backfill uses — same
// field names, same broadcast, same control-state persist — rather than
// growing a parallel read path that would drift. The refresh endpoint reaches
// this only after establishing the stream is stale, so the MYR-300 gate is a
// no-op for it by construction and the full field set flows.
//
// Because the read includes vehicle_config, an owner-triggered refresh also
// re-acquires the REST-only spec facts `trim` (MYR-279) and
// `seatCoolingCapable` (MYR-308) for free.
//
// It NEVER force-wakes — waking is the caller's decision (the event path
// deliberately does not; the refresh endpoint does, under the bounded
// commands.Executor budget). Returned errors wrap the sentinels above and are
// safe to log (redacted VIN + status; the P1 payload body is never surfaced).
func (m *ServiceStatusMonitor) RefreshFromVehicleData(ctx context.Context, vin, token string) error {
	data, err := m.reader.GetVehicleData(ctx, token, vin)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVehicleDataRead, err)
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
		return nil
	}
	if m.bus == nil {
		return nil // direct-handler unit tests construct the monitor without a bus
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
		return fmt.Errorf("%w: %w", ErrVehicleDataPublish, err)
	}
	m.logger.Info("service-status: populated owner controls from REST vehicle_data",
		slog.String("vin", redactVIN(vin)), slog.Int("fields", len(fields)))
	return nil
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
