package telemetry

import tpb "github.com/tnando/my-robo-taxi-telemetry/internal/telemetry/proto/tesla"

// shiftStateString converts a Tesla ShiftState enum to a human-readable
// single-letter gear indicator used throughout MyRoboTaxi.
func shiftStateString(ss tpb.ShiftState) string {
	switch ss {
	case tpb.ShiftState_ShiftStateP:
		return "P"
	case tpb.ShiftState_ShiftStateR:
		return "R"
	case tpb.ShiftState_ShiftStateN:
		return "N"
	case tpb.ShiftState_ShiftStateD:
		return "D"
	case tpb.ShiftState_ShiftStateInvalid:
		return "Invalid"
	case tpb.ShiftState_ShiftStateSNA:
		return "SNA"
	default:
		return "Unknown"
	}
}

// detailedChargeStateString converts a DetailedChargeStateValue enum to a
// human-readable string.
func detailedChargeStateString(cs tpb.DetailedChargeStateValue) string {
	switch cs {
	case tpb.DetailedChargeStateValue_DetailedChargeStateDisconnected:
		return "Disconnected"
	case tpb.DetailedChargeStateValue_DetailedChargeStateNoPower:
		return "NoPower"
	case tpb.DetailedChargeStateValue_DetailedChargeStateStarting:
		return "Starting"
	case tpb.DetailedChargeStateValue_DetailedChargeStateCharging:
		return "Charging"
	case tpb.DetailedChargeStateValue_DetailedChargeStateComplete:
		return "Complete"
	case tpb.DetailedChargeStateValue_DetailedChargeStateStopped:
		return "Stopped"
	default:
		return "Unknown"
	}
}

// chargingStateString converts the deprecated ChargingState enum to a string.
// Older firmware versions use this instead of DetailedChargeStateValue.
func chargingStateString(cs tpb.ChargingState) string {
	switch cs {
	case tpb.ChargingState_ChargeStateDisconnected:
		return "Disconnected"
	case tpb.ChargingState_ChargeStateNoPower:
		return "NoPower"
	case tpb.ChargingState_ChargeStateStarting:
		return "Starting"
	case tpb.ChargingState_ChargeStateCharging:
		return "Charging"
	case tpb.ChargingState_ChargeStateComplete:
		return "Complete"
	case tpb.ChargingState_ChargeStateStopped:
		return "Stopped"
	default:
		return "Unknown"
	}
}

// carTypeString converts a CarTypeValue enum to a human-readable model name.
func carTypeString(ct tpb.CarTypeValue) string {
	switch ct {
	case tpb.CarTypeValue_CarTypeModelS:
		return "Model S"
	case tpb.CarTypeValue_CarTypeModelX:
		return "Model X"
	case tpb.CarTypeValue_CarTypeModel3:
		return "Model 3"
	case tpb.CarTypeValue_CarTypeModelY:
		return "Model Y"
	case tpb.CarTypeValue_CarTypeSemiTruck:
		return "Semi"
	case tpb.CarTypeValue_CarTypeCybertruck:
		return "Cybertruck"
	default:
		return "Unknown"
	}
}

// sentryModeString converts a SentryModeState enum to a human-readable string.
func sentryModeString(sm tpb.SentryModeState) string {
	switch sm {
	case tpb.SentryModeState_SentryModeStateOff:
		return "Off"
	case tpb.SentryModeState_SentryModeStateIdle:
		return "Idle"
	case tpb.SentryModeState_SentryModeStateArmed:
		return "Armed"
	case tpb.SentryModeState_SentryModeStateAware:
		return "Aware"
	case tpb.SentryModeState_SentryModeStatePanic:
		return "Panic"
	case tpb.SentryModeState_SentryModeStateQuiet:
		return "Quiet"
	default:
		return "Unknown"
	}
}

// defrostModeString converts a DefrostModeState enum to a human-readable string.
func defrostModeString(dm tpb.DefrostModeState) string {
	switch dm {
	case tpb.DefrostModeState_DefrostModeStateOff:
		return "Off"
	case tpb.DefrostModeState_DefrostModeStateNormal:
		return "Normal"
	case tpb.DefrostModeState_DefrostModeStateMax:
		return "Max"
	case tpb.DefrostModeState_DefrostModeStateAutoDefog:
		return "AutoDefog"
	default:
		return "Unknown"
	}
}

// climateKeeperModeString converts a ClimateKeeperModeState enum to a
// human-readable string.
func climateKeeperModeString(ck tpb.ClimateKeeperModeState) string {
	switch ck {
	case tpb.ClimateKeeperModeState_ClimateKeeperModeStateOff:
		return "Off"
	case tpb.ClimateKeeperModeState_ClimateKeeperModeStateOn:
		return "On"
	case tpb.ClimateKeeperModeState_ClimateKeeperModeStateDog:
		return "Dog"
	case tpb.ClimateKeeperModeState_ClimateKeeperModeStateParty:
		return "Party"
	default:
		return "Unknown"
	}
}

// hvacPowerString converts an HvacPowerState enum to a human-readable string.
func hvacPowerString(hp tpb.HvacPowerState) string {
	switch hp {
	case tpb.HvacPowerState_HvacPowerStateOff:
		return "Off"
	case tpb.HvacPowerState_HvacPowerStateOn:
		return "On"
	case tpb.HvacPowerState_HvacPowerStatePrecondition:
		return "Precondition"
	case tpb.HvacPowerState_HvacPowerStateOverheatProtect:
		return "OverheatProtect"
	default:
		return "Unknown"
	}
}
