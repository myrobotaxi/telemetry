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
// re-acquires the REST-only spec facts `trim` (MYR-279), `seatCoolingCapable`
// (MYR-308) and `trimLabel` (MYR-320) for free, and writes the exterior colour
// to its own column. MYR-320 additionally issues ONE extra GET
// (/release_notes) here for `fsdVersion`, which no vehicle_data field carries;
// it is non-fatal and adds nothing when it fails.
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

	// MYR-320: the exterior colour goes to its OWN column (Prisma
	// "Vehicle".color), not into the telemetry frame — see syncVehicleColor.
	// Runs before the empty-frame short-circuit below so a car whose
	// vehicle_data carries nothing but a colour still gets it.
	m.syncVehicleColor(ctx, vin, data.VehicleConfig)

	// MYR-320: ONE additional non-waking GET on the same trigger, for the one
	// value no vehicle_data field carries. Non-fatal: a failure or an empty list
	// simply adds no field, leaving any stored fsdVersion alone.
	m.addFSDVersionField(ctx, vin, token, fields)

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
