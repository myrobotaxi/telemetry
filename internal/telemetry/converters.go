package telemetry

import (
	"fmt"
	"strconv"

	"github.com/myrobotaxi/telemetry/internal/events"
	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// convertValue dispatches to the appropriate converter based on the
// Tesla field's expected type. Tesla's proto Value is a big oneof;
// the actual variant used depends on both the field and the firmware
// version.
func convertValue(field tpb.Field, v *tpb.Value) (events.TelemetryValue, error) {
	switch field {
	case tpb.Field_Location, tpb.Field_OriginLocation, tpb.Field_DestinationLocation:
		return convertLocation(v)
	case tpb.Field_Gear:
		return convertShiftState(v)
	// MYR-42: chargeState sources from proto 179 DetailedChargeState only.
	// Proto 2 ChargeState is not in fieldMap and therefore not dispatched.
	case tpb.Field_DetailedChargeState:
		return convertChargeState(v)
	case tpb.Field_CarType:
		return convertCarType(v)
	case tpb.Field_SentryMode:
		return convertSentryMode(v)
	case tpb.Field_Locked:
		return convertBool(v)
	case tpb.Field_DefrostMode:
		return convertDefrostMode(v)
	case tpb.Field_ClimateKeeperMode:
		return convertClimateKeeperMode(v)
	case tpb.Field_HvacPower:
		return convertHvacPower(v)
	case tpb.Field_InsideTemp, tpb.Field_OutsideTemp,
		tpb.Field_HvacLeftTemperatureRequest, tpb.Field_HvacRightTemperatureRequest:
		return convertTemperature(v)
	case tpb.Field_RouteLine:
		return convertRouteLine(v)
	default:
		return convertNumericOrString(v)
	}
}

// convertLocation extracts a LocationValue. Tesla sends location fields
// using the locationValue variant of the Value oneof.
func convertLocation(v *tpb.Value) (events.TelemetryValue, error) {
	loc := v.GetLocationValue()
	if loc == nil {
		// Some firmware versions may send location as a string (very rare).
		if sv, ok := v.Value.(*tpb.Value_StringValue); ok {
			s := sv.StringValue
			return events.TelemetryValue{StringVal: &s}, nil
		}
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected locationValue, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
	return events.TelemetryValue{
		LocationVal: &events.Location{
			Latitude:  loc.GetLatitude(),
			Longitude: loc.GetLongitude(),
		},
	}, nil
}

// convertBool handles boolean fields (e.g., Locked).
func convertBool(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_BooleanValue:
		b := val.BooleanValue
		return events.TelemetryValue{BoolVal: &b}, nil
	case *tpb.Value_StringValue:
		b := val.StringValue == "true"
		return events.TelemetryValue{BoolVal: &b}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected bool or string, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
}

// convertNumericOrString handles the most common case: fields that should
// be numeric but may arrive as any of stringValue, floatValue, doubleValue,
// intValue, or longValue depending on firmware version.
//
// Tesla's biggest quirk: fields like VehicleSpeed, Odometer, InsideTemp
// are often sent as string_value ("65.2") rather than float/double. We
// parse strings into float64 and normalize all numeric variants to
// float64.
func convertNumericOrString(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_StringValue:
		return parseStringValue(val.StringValue)
	case *tpb.Value_FloatValue:
		f := float64(val.FloatValue)
		return events.TelemetryValue{FloatVal: &f}, nil
	case *tpb.Value_DoubleValue:
		return events.TelemetryValue{FloatVal: &val.DoubleValue}, nil
	case *tpb.Value_IntValue:
		i := int64(val.IntValue)
		return events.TelemetryValue{IntVal: &i}, nil
	case *tpb.Value_LongValue:
		return events.TelemetryValue{IntVal: &val.LongValue}, nil
	case *tpb.Value_BooleanValue:
		b := val.BooleanValue
		return events.TelemetryValue{BoolVal: &b}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected numeric or string, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
}

// convertRouteLine extracts a RouteLine string value. Tesla sends the nav
// route as a Google Encoded Polyline in a string_value. Any other Value type
// is unexpected and treated as an error.
func convertRouteLine(v *tpb.Value) (events.TelemetryValue, error) {
	if sv, ok := v.Value.(*tpb.Value_StringValue); ok {
		s := sv.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	}
	return events.TelemetryValue{}, fmt.Errorf(
		"%w: RouteLine expected string, got %T", ErrUnexpectedValueType, v.Value,
	)
}

// convertTemperature extracts a numeric Celsius value and converts it to
// Fahrenheit. Tesla sends temperatures in Celsius; the frontend expects
// Fahrenheit for US users, so we convert at the telemetry pipeline boundary
// so all downstream consumers (WebSocket, database) receive Fahrenheit.
func convertTemperature(v *tpb.Value) (events.TelemetryValue, error) {
	tv, err := convertNumericOrString(v)
	if err != nil {
		return tv, err
	}

	if tv.FloatVal != nil {
		f := celsiusToFahrenheit(*tv.FloatVal)
		return events.TelemetryValue{FloatVal: &f}, nil
	}

	return tv, nil
}

// celsiusToFahrenheit converts a Celsius temperature to Fahrenheit.
func celsiusToFahrenheit(c float64) float64 {
	return c*9.0/5.0 + 32.0
}

// parseStringValue attempts to parse a string into a numeric TelemetryValue.
// Tesla sends many numeric fields as strings ("65.2", "42", "0").
// We try parsing as float64, falling back to keeping it as a string
// for genuinely string-typed fields (VehicleName, Version, RouteLine, etc).
func parseStringValue(s string) (events.TelemetryValue, error) {
	if s == "" {
		return events.TelemetryValue{StringVal: &s}, nil
	}

	// Try parsing as float64. Most Tesla "string" numerics are floats.
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return events.TelemetryValue{FloatVal: &f}, nil
	}

	// Not a number, keep as string. This is valid for fields like
	// VehicleName, Version, RouteLine, DestinationName.
	return events.TelemetryValue{StringVal: &s}, nil
}
