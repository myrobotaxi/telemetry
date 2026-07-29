package telemetry

import (
	"fmt"
	"strconv"

	"github.com/myrobotaxi/telemetry/internal/events"
	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// fieldConverters maps a Tesla proto field to the converter for its value
// variant. Fields absent from the table fall back to convertNumericOrString
// (the common numeric-or-string case — speed, odometer, seat levels, media
// volume, etc.). A table keeps convertValue flat: adding a field is a
// one-line entry, not another switch case (which pushed cyclomatic
// complexity past the cyclop cap when MYR-252 added the cabin fields).
// Mirrors the existing package-level lookup tables (fieldMap, fields.go).
var fieldConverters = map[tpb.Field]func(*tpb.Value) (events.TelemetryValue, error){
	tpb.Field_Location:            convertLocation,
	tpb.Field_OriginLocation:      convertLocation,
	tpb.Field_DestinationLocation: convertLocation,
	tpb.Field_Gear:                convertShiftState,
	// MYR-42: chargeState sources from proto 179 DetailedChargeState only.
	// Proto 2 ChargeState is not in fieldMap and therefore not dispatched.
	tpb.Field_DetailedChargeState: convertChargeState,
	tpb.Field_CarType:             convertCarType,
	// MYR-279: keep the firmware Version as a string (never numeric-coerced).
	tpb.Field_Version:    convertVersion,
	tpb.Field_SentryMode: convertSentryMode,
	// MYR-252: boolean cabin-control fields. Locked (Group A) plus the
	// Group B HvacACEnabled / SeatVentEnabled / ChargePortDoorOpen.
	tpb.Field_Locked:             convertBool,
	tpb.Field_HvacACEnabled:      convertBool,
	tpb.Field_SeatVentEnabled:    convertBool,
	tpb.Field_ChargePortDoorOpen: convertBool,
	// MYR-259: ServiceMode (proto 159) is a boolean; some firmware sends it
	// as the string "true"/"false", both of which convertBool handles.
	tpb.Field_ServiceMode:       convertBool,
	tpb.Field_DefrostMode:       convertDefrostMode,
	tpb.Field_ClimateKeeperMode: convertClimateKeeperMode,
	tpb.Field_HvacPower:         convertHvacPower,
	// MYR-252 Group B enum + Doors decode.
	tpb.Field_HvacAutoMode:        convertHvacAutoMode,
	tpb.Field_MediaPlaybackStatus: convertMediaStatus,
	tpb.Field_DoorState:           convertDoorState,
	// MYR-303 free-text now-playing fields. These MUST be listed: without an
	// entry they fall through to convertNumericOrString, which ParseFloats a
	// numeric-looking title and returns a FloatVal with no StringVal — and both
	// consumers (store.mapControlMediaNowPlaying, the WS field mapping) read
	// StringVal only, so the value is silently dropped. See convertMediaText.
	tpb.Field_MediaNowPlayingTitle:   convertMediaText,
	tpb.Field_MediaNowPlayingArtist:  convertMediaText,
	tpb.Field_MediaNowPlayingAlbum:   convertMediaText,
	tpb.Field_MediaNowPlayingStation: convertMediaText,
	tpb.Field_MediaPlaybackSource:    convertMediaText,
	// Temperatures (incl. driver/passenger setpoints) — Celsius→Fahrenheit.
	tpb.Field_InsideTemp:                  convertTemperature,
	tpb.Field_OutsideTemp:                 convertTemperature,
	tpb.Field_HvacLeftTemperatureRequest:  convertTemperature,
	tpb.Field_HvacRightTemperatureRequest: convertTemperature,
	tpb.Field_RouteLine:                   convertRouteLine,
}

// convertValue dispatches to the appropriate converter based on the Tesla
// field's expected type. Tesla's proto Value is a big oneof; the actual
// variant used depends on both the field and the firmware version.
func convertValue(field tpb.Field, v *tpb.Value) (events.TelemetryValue, error) {
	if conv, ok := fieldConverters[field]; ok {
		return conv(v)
	}
	return convertNumericOrString(v)
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

// convertVersion keeps the firmware version as a string (MYR-279). Tesla sends
// Version as a string_value (e.g. "2026.20.1 9a8b7c6"); without a dedicated
// converter the numeric-or-string fallback would ParseFloat first, and would
// coerce a rare purely-numeric-looking version (e.g. "2026") to a float that the
// string-typed store column then drops. Forcing StringVal persists it verbatim.
func convertVersion(v *tpb.Value) (events.TelemetryValue, error) {
	if sv, ok := v.Value.(*tpb.Value_StringValue); ok {
		s := sv.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	}
	return convertNumericOrString(v)
}

// convertMediaText keeps the MYR-303 now-playing free-text fields as strings —
// the same guarantee convertVersion gives softwareVersion, and for the same
// reason.
//
// Media metadata is arbitrary human text that very often parses as a number:
// track titles ("1979", "22", "7"), album names ("21", "1989"), FM station
// labels ("94.7"). Without this converter the numeric-or-string fallback wins
// that ParseFloat race and hands back a FloatVal with StringVal nil. Every
// consumer of these five fields reads StringVal — the control-state persist
// (store.mapControlMediaNowPlaying) and the WS field mapping both do — so such a
// value is not merely mistyped, it VANISHES: nothing is persisted, nothing is
// broadcast, and the stored column stays at whatever it held, indistinguishable
// on read from "the car never told us".
//
// An EMPTY string is returned as a non-nil empty StringVal, never dropped: under
// MYR-303 semantics that is the car reporting the track ENDED, a real
// observation that must overwrite a stale title through the COALESCE upsert.
//
// Non-string Value variants fall back to convertNumericOrString rather than
// being forced into a string. Tesla sends these as string_value; inventing a
// rendering for a variant we have never observed would be guessing, and the
// fallback's behaviour for them is unchanged from before this converter existed.
func convertMediaText(v *tpb.Value) (events.TelemetryValue, error) {
	if sv, ok := v.Value.(*tpb.Value_StringValue); ok {
		s := sv.StringValue
		return events.TelemetryValue{StringVal: &s}, nil
	}
	return convertNumericOrString(v)
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
