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

// convertChargeState extracts the live `chargeState` value. As of MYR-42
// (2026-04-23) this is sourced from Tesla proto field 179
// (`DetailedChargeState`, emitted via the `Value_DetailedChargeStateValue`
// oneof variant). Older firmware still emits via the deprecated
// `Value_ChargingValue` variant; both paths produce the same 7 contract
// enum strings: Unknown, Disconnected, NoPower, Starting, Charging,
// Complete, Stopped. The string fallback covers the rare case where
// Tesla sends the value as a plain string.
//
// Tesla firmware ≥ 2024.44.25 does NOT populate proto 2 `ChargeState`
// even when configured, so that dispatch path is deliberately removed.
// See MYR-42 and `websocket-protocol.md` §10 DV-19.
func convertChargeState(v *tpb.Value) (events.TelemetryValue, error) {
	switch val := v.Value.(type) {
	case *tpb.Value_DetailedChargeStateValue:
		s := detailedChargeStateString(val.DetailedChargeStateValue)
		return events.TelemetryValue{StringVal: &s}, nil
	case *tpb.Value_ChargingValue:
		// Pre-2024.44.25 firmware emits the deprecated ChargingState enum
		// instead. Same string values, narrower enum range.
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
