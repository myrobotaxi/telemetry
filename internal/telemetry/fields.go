package telemetry

import (
	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// FieldName is the internal name for a telemetry field. These are the
// canonical names used throughout the system (events, store, WebSocket
// messages) regardless of how Tesla names or encodes them.
type FieldName string

// Internal field names used by MyRoboTaxi. Downstream consumers (drive
// detector, WebSocket broadcast, store) reference these constants, not
// Tesla's proto enum names.
const (
	FieldSpeed                FieldName = "speed"
	FieldLocation             FieldName = "location"
	FieldHeading              FieldName = "heading"
	FieldGear                 FieldName = "gear"
	FieldSOC                  FieldName = "soc"
	FieldEstBatteryRange      FieldName = "estimatedRange"
	FieldChargeState          FieldName = "chargeState"
	FieldTimeToFull           FieldName = "timeToFull"
	FieldOdometer             FieldName = "odometer"
	FieldInsideTemp           FieldName = "insideTemp"
	FieldOutsideTemp          FieldName = "outsideTemp"
	FieldHvacPower            FieldName = "hvacPower"
	FieldHvacFanSpeed         FieldName = "hvacFanSpeed"
	FieldDriverTempSetting    FieldName = "driverTempSetting"
	FieldPassengerTempSetting FieldName = "passengerTempSetting"
	FieldDefrostMode          FieldName = "defrostMode"
	FieldSeatHeaterLeft       FieldName = "seatHeaterLeft"
	FieldSeatHeaterRight      FieldName = "seatHeaterRight"
	FieldClimateKeeperMode    FieldName = "climateKeeperMode"
	FieldDestinationName      FieldName = "destinationName"
	FieldRouteLine            FieldName = "routeLine"
	FieldFSDMiles             FieldName = "fsdMilesSinceReset"
	FieldBatteryLevel         FieldName = "batteryLevel"
	FieldIdealBatteryRange    FieldName = "idealBatteryRange"
	FieldRatedRange           FieldName = "ratedRange"
	FieldEnergyRemaining      FieldName = "energyRemaining"
	FieldPackVoltage          FieldName = "packVoltage"
	FieldPackCurrent          FieldName = "packCurrent"
	FieldVehicleName          FieldName = "vehicleName"
	FieldCarType              FieldName = "carType"
	FieldVersion              FieldName = "version"
	// FieldTrim (MYR-279) is an INTERNAL-only field name (no Tesla proto / no
	// fieldMap entry): Tesla does NOT stream the trim badge, so it is sourced
	// ONLY from REST vehicle_data.vehicle_config.trim_badging by the MYR-260
	// /vehicle_data backfill, routed through the same control-state persist path
	// as the streamed fields, and surfaced on the REST /snapshot as `trim`.
	FieldTrim             FieldName = "trim"
	FieldLocked           FieldName = "locked"
	FieldSentryMode       FieldName = "sentryMode"
	FieldOriginLocation   FieldName = "originLocation"
	FieldDestLocation     FieldName = "destinationLocation"
	FieldMilesToArrival   FieldName = "milesToArrival"
	FieldMinutesToArrival FieldName = "minutesToArrival"
	FieldLatAccel         FieldName = "lateralAcceleration"
	FieldLongAccel        FieldName = "longitudinalAcceleration"
	FieldMilesSinceReset  FieldName = "milesSinceReset"
	// MYR-252 Group B cabin-control read-back internal names. These equal
	// the wire field names in vehicle-state.schema.json so mapFieldsForClient
	// passes them through unchanged — except doorState, which is bit-decoded
	// into frunkOpen/trunkOpen in internal/ws (see converters_doors.go).
	FieldHvacAutoMode         FieldName = "hvacAutoMode"
	FieldHvacACEnabled        FieldName = "hvacAcEnabled"
	FieldSeatHeaterRearLeft   FieldName = "seatHeaterRearLeft"
	FieldSeatHeaterRearCenter FieldName = "seatHeaterRearCenter"
	FieldSeatHeaterRearRight  FieldName = "seatHeaterRearRight"
	FieldSeatCoolerLeft       FieldName = "seatCoolerLeft"
	FieldSeatCoolerRight      FieldName = "seatCoolerRight"
	FieldSeatVentEnabled      FieldName = "seatVentEnabled"
	FieldChargePortDoorOpen   FieldName = "chargePortDoorOpen"
	FieldDoorState            FieldName = "doorState"
	FieldMediaPlaybackStatus  FieldName = "mediaPlaybackStatus"
	FieldMediaVolume          FieldName = "mediaVolume"
	// MYR-303 media now-playing internal names. Each EQUALS its wire field name
	// in vehicle-state.schema.json, so internal/ws passes them through unchanged
	// and no internalToClientField entry is needed — the same convention the
	// MYR-252 media siblings above use. Two names are deliberately NOT the proto
	// name, and the contraction happens HERE (in fieldMap below) rather than in
	// the WS translate table, exactly as MediaAudioVolume (244) became
	// `mediaVolume`:
	//   - MediaAudioVolumeMax (252)     → mediaVolumeMax
	//   - MediaNowPlayingDuration (245) → mediaNowPlayingDurationMs
	//   - MediaNowPlayingElapsed (246)  → mediaNowPlayingElapsedMs
	// The `Ms` suffix is load-bearing: the contract fixes the unit at
	// milliseconds, and the bare proto names state no unit.
	FieldMediaTitle          FieldName = "mediaNowPlayingTitle"
	FieldMediaArtist         FieldName = "mediaNowPlayingArtist"
	FieldMediaAlbum          FieldName = "mediaNowPlayingAlbum"
	FieldMediaStation        FieldName = "mediaNowPlayingStation"
	FieldMediaPlaybackSource FieldName = "mediaPlaybackSource"
	FieldMediaDurationMs     FieldName = "mediaNowPlayingDurationMs"
	FieldMediaElapsedMs      FieldName = "mediaNowPlayingElapsedMs"
	FieldMediaVolumeMax      FieldName = "mediaVolumeMax"
	// FieldSeatCoolingCapable (MYR-308) is an INTERNAL-only field name with NO
	// Tesla proto and NO fieldMap entry, exactly like FieldTrim above. Tesla does
	// not stream a ventilated-seat capability bit; it is sourced ONLY from REST
	// vehicle_data.vehicle_config.has_seat_cooling by the MYR-260 backfill,
	// routed through the same control-state persist path as the streamed fields,
	// and surfaced on the REST /snapshot as `seatCoolingCapable`.
	//
	// Keeping it OUT of fieldMap is what carries it past the MYR-300
	// stream-recency gate: streamSourcedFields (service_status_stream_freshness.go)
	// is built from fieldMap, and dropStreamSourcedFields only deletes fields in
	// that set. A REST-only field is therefore never dropped, so a busily-
	// streaming car can still acquire the capability — and an in-service car,
	// which is the whole point of MYR-308, has no fresh stream stamp at all.
	FieldSeatCoolingCapable FieldName = "seatCoolingCapable"
	// FieldServiceMode is Tesla proto 159 (ServiceMode, bool). MYR-259: an
	// INTERNAL-only signal — decoded and fed into status derivation
	// (in_service) by internal/ws, but never emitted as its own wire field
	// (stripped before broadcast in ensureGearGroupAtomic). See
	// vehicle-state-schema.md §2.4 and data-classification.md.
	FieldServiceMode FieldName = "serviceMode"
)

// fieldMap maps Tesla's proto Field enum values to our internal field names.
// Only fields that MyRoboTaxi cares about are included. Unlisted fields are
// silently skipped during decoding.
var fieldMap = map[tpb.Field]FieldName{
	tpb.Field_VehicleSpeed:    FieldSpeed,
	tpb.Field_Location:        FieldLocation,
	tpb.Field_GpsHeading:      FieldHeading,
	tpb.Field_Gear:            FieldGear,
	tpb.Field_Soc:             FieldSOC,
	tpb.Field_EstBatteryRange: FieldEstBatteryRange,
	// MYR-42 (2026-04-23): chargeState sources from proto 179 DetailedChargeState,
	// NOT proto 2 ChargeState. Empirical capture showed Tesla's recent firmware
	// (≥ 2024.44.25) accepts proto 2 in fleet_telemetry_config (synced: true)
	// but never actually emits it, even across plug/unplug transitions. Proto
	// 179 fires on the same transitions with identical enum string values, so
	// routing it to the FieldChargeState internal name keeps the wire contract
	// unchanged. Field_ChargeState (proto 2) is intentionally NOT in fieldMap.
	tpb.Field_DetailedChargeState:         FieldChargeState,
	tpb.Field_TimeToFullCharge:            FieldTimeToFull,
	tpb.Field_Odometer:                    FieldOdometer,
	tpb.Field_InsideTemp:                  FieldInsideTemp,
	tpb.Field_OutsideTemp:                 FieldOutsideTemp,
	tpb.Field_HvacPower:                   FieldHvacPower,
	tpb.Field_HvacFanSpeed:                FieldHvacFanSpeed,
	tpb.Field_HvacLeftTemperatureRequest:  FieldDriverTempSetting,
	tpb.Field_HvacRightTemperatureRequest: FieldPassengerTempSetting,
	tpb.Field_DefrostMode:                 FieldDefrostMode,
	tpb.Field_SeatHeaterLeft:              FieldSeatHeaterLeft,
	tpb.Field_SeatHeaterRight:             FieldSeatHeaterRight,
	tpb.Field_ClimateKeeperMode:           FieldClimateKeeperMode,
	tpb.Field_DestinationName:             FieldDestinationName,
	tpb.Field_RouteLine:                   FieldRouteLine,
	tpb.Field_SelfDrivingMilesSinceReset:  FieldFSDMiles,
	tpb.Field_BatteryLevel:                FieldBatteryLevel,
	tpb.Field_IdealBatteryRange:           FieldIdealBatteryRange,
	tpb.Field_RatedRange:                  FieldRatedRange,
	tpb.Field_EnergyRemaining:             FieldEnergyRemaining,
	tpb.Field_PackVoltage:                 FieldPackVoltage,
	tpb.Field_PackCurrent:                 FieldPackCurrent,
	tpb.Field_VehicleName:                 FieldVehicleName,
	tpb.Field_CarType:                     FieldCarType,
	tpb.Field_Version:                     FieldVersion,
	tpb.Field_Locked:                      FieldLocked,
	tpb.Field_SentryMode:                  FieldSentryMode,
	tpb.Field_OriginLocation:              FieldOriginLocation,
	tpb.Field_DestinationLocation:         FieldDestLocation,
	tpb.Field_MilesToArrival:              FieldMilesToArrival,
	tpb.Field_MinutesToArrival:            FieldMinutesToArrival,
	tpb.Field_LateralAcceleration:         FieldLatAccel,
	tpb.Field_LongitudinalAcceleration:    FieldLongAccel,
	tpb.Field_MilesSinceReset:             FieldMilesSinceReset,
	// MYR-252 Group B cabin-control read-back. Group A climate/lock/seat
	// fields (HvacPower, HvacFanSpeed, Hvac{Left,Right}TemperatureRequest,
	// Locked, SeatHeater{Left,Right}) were already mapped above; MYR-252
	// contracts them + adds them to the owner mask allow-list.
	tpb.Field_HvacAutoMode:                 FieldHvacAutoMode,
	tpb.Field_HvacACEnabled:                FieldHvacACEnabled,
	tpb.Field_SeatHeaterRearLeft:           FieldSeatHeaterRearLeft,
	tpb.Field_SeatHeaterRearCenter:         FieldSeatHeaterRearCenter,
	tpb.Field_SeatHeaterRearRight:          FieldSeatHeaterRearRight,
	tpb.Field_ClimateSeatCoolingFrontLeft:  FieldSeatCoolerLeft,
	tpb.Field_ClimateSeatCoolingFrontRight: FieldSeatCoolerRight,
	tpb.Field_SeatVentEnabled:              FieldSeatVentEnabled,
	tpb.Field_ChargePortDoorOpen:           FieldChargePortDoorOpen,
	tpb.Field_DoorState:                    FieldDoorState,
	tpb.Field_MediaPlaybackStatus:          FieldMediaPlaybackStatus,
	tpb.Field_MediaAudioVolume:             FieldMediaVolume,
	// MYR-303 media now-playing. Proto numbers verified against the vendored
	// vehicle_data.proto: 248/247/249/250 (title/artist/album/station), 243
	// (playback source), 245/246 (duration/elapsed, milliseconds), 252 (volume
	// ceiling). See the FieldMedia* block above for the two name contractions.
	tpb.Field_MediaNowPlayingTitle:    FieldMediaTitle,
	tpb.Field_MediaNowPlayingArtist:   FieldMediaArtist,
	tpb.Field_MediaNowPlayingAlbum:    FieldMediaAlbum,
	tpb.Field_MediaNowPlayingStation:  FieldMediaStation,
	tpb.Field_MediaPlaybackSource:     FieldMediaPlaybackSource,
	tpb.Field_MediaNowPlayingDuration: FieldMediaDurationMs,
	tpb.Field_MediaNowPlayingElapsed:  FieldMediaElapsedMs,
	tpb.Field_MediaAudioVolumeMax:     FieldMediaVolumeMax,
	// Field_MediaAudioVolumeIncrement (251) is intentionally NOT mapped — the
	// contract has no wire field for the volume STEP, only the current level
	// (mediaVolume) and the ceiling (mediaVolumeMax). Adding it here would leak
	// an uncontracted field to WS clients via the event bus.
	//
	// FieldSeatCoolingCapable (MYR-308) is likewise absent BY DESIGN: it has no
	// proto at all and is REST-only, which is what keeps it out of
	// streamSourcedFields and therefore past the MYR-300 gate. See fields.go's
	// FieldSeatCoolingCapable comment.
	// MYR-259: ServiceMode (proto 159, bool). Decoded so the broadcaster can
	// derive status=in_service; the raw signal is stripped before it reaches
	// the wire (internal/ws/nav_broadcast.go), so no uncontracted field leaks.
	tpb.Field_ServiceMode: FieldServiceMode,
	// Field_EstimatedHoursToChargeTermination (190) is intentionally NOT in fieldMap
	// — it remains observation-only via the MYR-25 debug log in decoder.go pending
	// the Trip Planner Supercharger capture that MYR-28's §7.1 flip condition
	// requires. Adding it here would leak an uncontracted field to WS clients via
	// the event bus. Promote once the MYR-25 capture confirms MYR-28's proto-43
	// decision does not flip.
	// Field_RouteLastUpdated omitted — Tesla docs state this field is broken.
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
