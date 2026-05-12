package telemetry

import (
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/events"
	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// DecodeRawPayload walks every datum in a Tesla Payload and produces a
// RawVehicleTelemetryEvent preserving the proto field number, enum name,
// and raw value. Unlike DecodePayload, this method does NOT drop fields
// that are missing from InternalFieldName — it surfaces the full firmware
// view so developers can inspect untracked fields from the cmd/ops CLI or
// the debug WebSocket endpoint.
//
// Datums with decode errors are skipped (the normal decode path logs them).
// Datums marked Invalid are emitted with Invalid=true and a nil Value.
func (d *Decoder) DecodeRawPayload(payload *tpb.Payload) (events.RawVehicleTelemetryEvent, error) {
	if err := validatePayload(payload); err != nil {
		return events.RawVehicleTelemetryEvent{}, err
	}

	fields := make([]events.RawTelemetryField, 0, len(payload.GetData()))
	for _, datum := range payload.GetData() {
		if datum == nil {
			continue
		}
		raw, ok := rawFieldFromDatum(datum)
		if !ok {
			continue
		}
		fields = append(fields, raw)
	}

	return events.RawVehicleTelemetryEvent{
		VIN:       payload.GetVin(),
		CreatedAt: payload.GetCreatedAt().AsTime(),
		Fields:    fields,
	}, nil
}

// rawFieldFromDatum extracts the raw value from a Datum without the
// fieldMap filtering or unit conversion applied by the normal decode path.
// Returns ok=false for datums that fail to decode; the caller skips them.
func rawFieldFromDatum(datum *tpb.Datum) (events.RawTelemetryField, bool) {
	key := datum.GetKey()
	base := events.RawTelemetryField{
		ProtoField: int32(key),
		ProtoName:  key.String(),
	}

	v := datum.GetValue()
	if v == nil {
		return events.RawTelemetryField{}, false
	}

	if _, invalid := v.Value.(*tpb.Value_Invalid); invalid {
		base.Invalid = true
		base.Type = "invalid"
		return base, true
	}

	typ, value := rawValue(v)
	base.Type = typ
	base.Value = value
	return base, true
}

// rawValue returns the raw Go value for a Tesla Value oneof, preserving the
// original representation (no Celsius→Fahrenheit conversion, no enum-to-string
// shortening). Strings that happen to parse as numbers are kept as strings so
// the developer sees what the vehicle actually sent.
func rawValue(v *tpb.Value) (typeTag string, value any) {
	if typeTag, value, ok := rawScalarValue(v); ok {
		return typeTag, value
	}
	if typeTag, value, ok := rawEnumValue(v); ok {
		return typeTag, value
	}
	// Firmware may send typed oneof variants we do not yet map (e.g.
	// newly added enums). Surface them as a generic string so the
	// developer can still see what the vehicle sent.
	return "unknown", fmt.Sprintf("%v", v.Value)
}

// rawScalarValue handles the primitive oneof variants (string/numeric/bool
// and the location pair).
func rawScalarValue(v *tpb.Value) (typeTag string, value any, ok bool) {
	switch val := v.Value.(type) {
	case *tpb.Value_StringValue:
		return "string", val.StringValue, true
	case *tpb.Value_FloatValue:
		return "float", float64(val.FloatValue), true
	case *tpb.Value_DoubleValue:
		return "double", val.DoubleValue, true
	case *tpb.Value_IntValue:
		return "int", int64(val.IntValue), true
	case *tpb.Value_LongValue:
		return "long", val.LongValue, true
	case *tpb.Value_BooleanValue:
		return "bool", val.BooleanValue, true
	case *tpb.Value_LocationValue:
		loc := val.LocationValue
		return "location", events.Location{
			Latitude:  loc.GetLatitude(),
			Longitude: loc.GetLongitude(),
		}, true
	}
	return "", nil, false
}

// rawEnumValue stringifies the typed enum oneof variants using the same
// helpers as the main decoder. Returns ok=false when the variant is not an
// enum type the raw decoder knows about.
func rawEnumValue(v *tpb.Value) (typeTag string, value any, ok bool) {
	switch val := v.Value.(type) {
	case *tpb.Value_ShiftStateValue:
		return "enum.shift_state", shiftStateString(val.ShiftStateValue), true
	case *tpb.Value_DetailedChargeStateValue:
		return "enum.detailed_charge_state", detailedChargeStateString(val.DetailedChargeStateValue), true
	case *tpb.Value_ChargingValue:
		return "enum.charging_state", chargingStateString(val.ChargingValue), true
	case *tpb.Value_CarTypeValue:
		return "enum.car_type", carTypeString(val.CarTypeValue), true
	case *tpb.Value_SentryModeStateValue:
		return "enum.sentry_mode", sentryModeString(val.SentryModeStateValue), true
	case *tpb.Value_DefrostModeValue:
		return "enum.defrost_mode", defrostModeString(val.DefrostModeValue), true
	case *tpb.Value_ClimateKeeperModeValue:
		return "enum.climate_keeper_mode", climateKeeperModeString(val.ClimateKeeperModeValue), true
	case *tpb.Value_HvacPowerValue:
		return "enum.hvac_power", hvacPowerString(val.HvacPowerValue), true
	}
	return "", nil, false
}
