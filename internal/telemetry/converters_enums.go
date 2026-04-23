package telemetry

import (
	"fmt"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	tpb "github.com/tnando/my-robo-taxi-telemetry/internal/telemetry/proto/tesla"
)

// This file groups the Tesla-enum-to-string converters. Numeric/location/
// temperature/route-line converters live in converters.go; dispatch lives
// there too. Keeping enums here keeps either file under the CLAUDE.md
// 300-line cap and makes it obvious where to add a new enum routing case.

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

// convertChargeState extracts Tesla's proto 2 ChargeState enum (emitted
// via the `charging_value` oneof variant, which wraps the ChargingState
// enum) and returns it as a string. Produced values match the v1 charge
// atomic group contract: Unknown, Disconnected, NoPower, Starting,
// Charging, Complete, Stopped.
func convertChargeState(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_ChargingValue:
		s := chargingStateString(val.ChargingValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_StringValue:
		s := val.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected chargingState or string, got %T", ErrUnexpectedValueType, v.Value,
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

// convertDefrostMode extracts a DefrostModeState enum and returns it as a string.
func convertDefrostMode(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_DefrostModeValue:
		s := defrostModeString(val.DefrostModeValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_StringValue:
		s := val.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected defrostMode or string, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
}

// convertClimateKeeperMode extracts a ClimateKeeperModeState enum and returns
// it as a string.
func convertClimateKeeperMode(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_ClimateKeeperModeValue:
		s := climateKeeperModeString(val.ClimateKeeperModeValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_StringValue:
		s := val.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected climateKeeperMode or string, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
}

// convertHvacPower extracts an HvacPowerState enum and returns it as a string.
func convertHvacPower(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_HvacPowerValue:
		s := hvacPowerString(val.HvacPowerValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_StringValue:
		s := val.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	default:
		return events.TelemetryValue{}, fmt.Errorf(
			"%w: expected hvacPower or string, got %T", ErrUnexpectedValueType, v.Value,
		)
	}
}
