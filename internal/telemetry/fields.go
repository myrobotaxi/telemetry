package telemetry

import (
	tpb "github.com/tnando/my-robo-taxi-telemetry/internal/telemetry/proto/tesla"
)

// FieldName is the internal name for a telemetry field. These are the
// canonical names used throughout the system (events, store, WebSocket
// messages) regardless of how Tesla names or encodes them.
type FieldName string

// Internal field names used by MyRoboTaxi. Downstream consumers (drive
// detector, WebSocket broadcast, store) reference these constants, not
// Tesla's proto enum names.
const (
	FieldSpeed              FieldName = "speed"
	FieldLocation           FieldName = "location"
	FieldHeading            FieldName = "heading"
	FieldGear               FieldName = "gear"
	FieldSOC                FieldName = "soc"
	FieldEstBatteryRange    FieldName = "estimatedRange"
	FieldChargeState        FieldName = "chargeState"
	FieldOdometer           FieldName = "odometer"
	FieldInsideTemp         FieldName = "insideTemp"
	FieldOutsideTemp        FieldName = "outsideTemp"
	FieldDestinationName    FieldName = "destinationName"
	FieldRouteLine          FieldName = "routeLine"
	FieldFSDMiles           FieldName = "fsdMilesSinceReset"
	FieldBatteryLevel       FieldName = "batteryLevel"
	FieldIdealBatteryRange  FieldName = "idealBatteryRange"
	FieldRatedRange         FieldName = "ratedRange"
	FieldEnergyRemaining    FieldName = "energyRemaining"
	FieldPackVoltage        FieldName = "packVoltage"
	FieldPackCurrent        FieldName = "packCurrent"
	FieldVehicleName        FieldName = "vehicleName"
	FieldCarType            FieldName = "carType"
	FieldVersion            FieldName = "version"
	FieldLocked             FieldName = "locked"
	FieldSentryMode         FieldName = "sentryMode"
	FieldOriginLocation     FieldName = "originLocation"
	FieldDestLocation       FieldName = "destinationLocation"
	FieldMilesToArrival     FieldName = "milesToArrival"
	FieldMinutesToArrival   FieldName = "minutesToArrival"
	FieldLatAccel            FieldName = "lateralAcceleration"
	FieldLongAccel           FieldName = "longitudinalAcceleration"
	FieldMilesSinceReset    FieldName = "milesSinceReset"
)

// fieldMap maps Tesla's proto Field enum values to our internal field names.
// Only fields that MyRoboTaxi cares about are included. Unlisted fields are
// silently skipped during decoding.
var fieldMap = map[tpb.Field]FieldName{
	tpb.Field_VehicleSpeed:             FieldSpeed,
	tpb.Field_Location:                 FieldLocation,
	tpb.Field_GpsHeading:               FieldHeading,
	tpb.Field_Gear:                     FieldGear,
	tpb.Field_Soc:                      FieldSOC,
	tpb.Field_EstBatteryRange:          FieldEstBatteryRange,
	tpb.Field_DetailedChargeState:      FieldChargeState,
	tpb.Field_Odometer:                 FieldOdometer,
	tpb.Field_InsideTemp:               FieldInsideTemp,
	tpb.Field_OutsideTemp:              FieldOutsideTemp,
	tpb.Field_DestinationName:          FieldDestinationName,
	tpb.Field_RouteLine:                FieldRouteLine,
	tpb.Field_SelfDrivingMilesSinceReset: FieldFSDMiles,
	tpb.Field_BatteryLevel:             FieldBatteryLevel,
	tpb.Field_IdealBatteryRange:        FieldIdealBatteryRange,
	tpb.Field_RatedRange:               FieldRatedRange,
	tpb.Field_EnergyRemaining:          FieldEnergyRemaining,
	tpb.Field_PackVoltage:              FieldPackVoltage,
	tpb.Field_PackCurrent:              FieldPackCurrent,
	tpb.Field_VehicleName:              FieldVehicleName,
	tpb.Field_CarType:                  FieldCarType,
	tpb.Field_Version:                  FieldVersion,
	tpb.Field_Locked:                   FieldLocked,
	tpb.Field_SentryMode:               FieldSentryMode,
	tpb.Field_OriginLocation:           FieldOriginLocation,
	tpb.Field_DestinationLocation:      FieldDestLocation,
	tpb.Field_MilesToArrival:           FieldMilesToArrival,
	tpb.Field_MinutesToArrival:         FieldMinutesToArrival,
	tpb.Field_LateralAcceleration:      FieldLatAccel,
	tpb.Field_LongitudinalAcceleration: FieldLongAccel,
	tpb.Field_MilesSinceReset:          FieldMilesSinceReset,
}

// IsTrackedField reports whether the given Tesla proto field is one that
// MyRoboTaxi decodes and processes. Fields not in the map are silently
// dropped.
func IsTrackedField(f tpb.Field) bool {
	_, ok := fieldMap[f]
	return ok
}

// InternalFieldName returns the internal field name for a Tesla proto field.
// Returns empty string and false if the field is not tracked.
func InternalFieldName(f tpb.Field) (FieldName, bool) {
	name, ok := fieldMap[f]
	return name, ok
}
