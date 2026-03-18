package telemetry

import (
	"fmt"
	"strconv"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	tpb "github.com/tnando/my-robo-taxi-telemetry/internal/telemetry/proto/tesla"
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
	case tpb.Field_DetailedChargeState:
		return convertDetailedChargeState(v)
	case tpb.Field_CarType:
		return convertCarType(v)
	case tpb.Field_SentryMode:
		return convertSentryMode(v)
	case tpb.Field_Locked:
		return convertBool(v)
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

// convertShiftState extracts a ShiftState enum and returns it as a string.
// Tesla uses the shift_state_value oneof variant, but older firmware may
// send it as a string.
func convertShiftState(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_ShiftStateValue:
		s := shiftStateString(val.ShiftStateValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_StringValue:
		s := val.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected shiftState or string, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
}

// convertDetailedChargeState extracts a DetailedChargeStateValue enum and
// returns it as a string.
func convertDetailedChargeState(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_DetailedChargeStateValue:
		s := detailedChargeStateString(val.DetailedChargeStateValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_ChargingValue:
		// Older firmware uses the deprecated ChargingState enum.
		s := chargingStateString(val.ChargingValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_StringValue:
		s := val.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected chargeState or string, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
}

// convertCarType extracts a CarTypeValue enum and returns it as a string.
func convertCarType(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_CarTypeValue:
		s := carTypeString(val.CarTypeValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_StringValue:
		s := val.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected carType or string, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
}

// convertSentryMode extracts a SentryModeState enum and returns it as a string.
func convertSentryMode(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_SentryModeStateValue:
		s := sentryModeString(val.SentryModeStateValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_StringValue:
		s := val.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected sentryModeState or string, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
}

// convertBool handles boolean fields (e.g., Locked).
func convertBool(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_BooleanValue:
		s := strconv.FormatBool(val.BooleanValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_StringValue:
		s := val.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
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
		s := strconv.FormatBool(val.BooleanValue)
		return events.TelemetryValue{StringVal: &s}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected numeric or string, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
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
