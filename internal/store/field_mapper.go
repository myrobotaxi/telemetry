package store

import (
	"math"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

// fieldApplier applies a TelemetryValue to the matching field on a
// VehicleUpdate. Returns true if the value was applied.
type fieldApplier func(u *VehicleUpdate, val events.TelemetryValue) bool

// fieldAppliers maps each tracked telemetry field to its applier function.
var fieldAppliers = map[telemetry.FieldName]fieldApplier{
	telemetry.FieldSpeed:           applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.Speed }),
	telemetry.FieldHeading:         applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.Heading }),
	telemetry.FieldSOC:             applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.ChargeLevel }),
	telemetry.FieldBatteryLevel:    applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.ChargeLevel }),
	telemetry.FieldEstBatteryRange: applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.EstimatedRange }),
	telemetry.FieldInsideTemp:      applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.InteriorTemp }),
	telemetry.FieldOutsideTemp:     applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.ExteriorTemp }),
	telemetry.FieldOdometer:        applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.OdometerMiles }),
	telemetry.FieldGear:            applyString(func(u *VehicleUpdate) **string { return &u.GearPosition }),
	telemetry.FieldLocation:        applyLocation,
	telemetry.FieldDestinationName: applyString(func(u *VehicleUpdate) **string { return &u.DestinationName }),
	telemetry.FieldDestLocation:    applyDestLocation,
	telemetry.FieldOriginLocation:  applyOriginLocation,
}

// mapTelemetryToUpdate converts a map of telemetry field values into a
// VehicleUpdate with only the present fields set. Fields not recognized
// or missing from the map are left nil (no-op on the database update).
func mapTelemetryToUpdate(fields map[string]events.TelemetryValue) *VehicleUpdate {
	u := &VehicleUpdate{}
	hasFields := false

	for name, val := range fields {
		apply, ok := fieldAppliers[telemetry.FieldName(name)]
		if !ok {
			continue
		}
		if apply(u, val) {
			hasFields = true
		}
	}

	if !hasFields {
		return nil
	}
	return u
}

// applyFloatAsInt returns an applier that rounds a float value to int and
// assigns it to the field returned by the target function.
func applyFloatAsInt(target func(u *VehicleUpdate) **int) fieldApplier {
	return func(u *VehicleUpdate, val events.TelemetryValue) bool {
		v := floatToIntPtr(val.FloatVal)
		if v == nil {
			return false
		}
		*target(u) = v
		return true
	}
}

// applyString returns an applier that assigns a string value to the field
// returned by the target function.
func applyString(target func(u *VehicleUpdate) **string) fieldApplier {
	return func(u *VehicleUpdate, val events.TelemetryValue) bool {
		if val.StringVal == nil {
			return false
		}
		*target(u) = val.StringVal
		return true
	}
}

// applyLocation applies a LocationVal to Latitude and Longitude fields.
// The pointers reference the event payload struct, which is safe because
// events are immutable after publish.
func applyLocation(u *VehicleUpdate, val events.TelemetryValue) bool {
	if val.LocationVal == nil {
		return false
	}
	u.Latitude = &val.LocationVal.Latitude
	u.Longitude = &val.LocationVal.Longitude
	return true
}

// applyDestLocation applies a LocationVal to DestinationLatitude and
// DestinationLongitude fields.
func applyDestLocation(u *VehicleUpdate, val events.TelemetryValue) bool {
	if val.LocationVal == nil {
		return false
	}
	u.DestinationLatitude = &val.LocationVal.Latitude
	u.DestinationLongitude = &val.LocationVal.Longitude
	return true
}

// applyOriginLocation applies a LocationVal to OriginLatitude and
// OriginLongitude fields.
func applyOriginLocation(u *VehicleUpdate, val events.TelemetryValue) bool {
	if val.LocationVal == nil {
		return false
	}
	u.OriginLatitude = &val.LocationVal.Latitude
	u.OriginLongitude = &val.LocationVal.Longitude
	return true
}

// floatToIntPtr rounds a float64 to the nearest int and returns a pointer.
// Returns nil if the input pointer is nil.
func floatToIntPtr(f *float64) *int {
	if f == nil {
		return nil
	}
	v := int(math.Round(*f))
	return &v
}
