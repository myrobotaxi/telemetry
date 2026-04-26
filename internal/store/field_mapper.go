package store

import (
	"encoding/json"
	"log/slog"
	"math"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/polyline"
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
	telemetry.FieldChargeState:     applyString(func(u *VehicleUpdate) **string { return &u.ChargeState }),
	telemetry.FieldTimeToFull:      applyFloat(func(u *VehicleUpdate) **float64 { return &u.TimeToFull }),
	telemetry.FieldInsideTemp:      applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.InteriorTemp }),
	telemetry.FieldOutsideTemp:     applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.ExteriorTemp }),
	telemetry.FieldOdometer:        applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.OdometerMiles }),
	telemetry.FieldGear:            applyString(func(u *VehicleUpdate) **string { return &u.GearPosition }),
	telemetry.FieldLocation:        applyLocation,
	telemetry.FieldDestinationName: applyString(func(u *VehicleUpdate) **string { return &u.DestinationName }),
	telemetry.FieldDestLocation:    applyDestLocation,
	telemetry.FieldOriginLocation:  applyOriginLocation,
	telemetry.FieldMinutesToArrival: applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.EtaMinutes }),
	telemetry.FieldMilesToArrival:   applyFloat(func(u *VehicleUpdate) **float64 { return &u.TripDistRemaining }),
	telemetry.FieldRouteLine:        applyRouteLine,
}

// navFieldColumns maps internal telemetry field names to the DB column
// names that should be SET NULL when the vehicle marks the field invalid
// (e.g. navigation cancelled).
var navFieldColumns = map[telemetry.FieldName][]string{
	telemetry.FieldDestinationName: {"destinationName"},
	telemetry.FieldMinutesToArrival: {"etaMinutes"},
	telemetry.FieldMilesToArrival:   {"tripDistanceRemaining"},
	telemetry.FieldOriginLocation:   {"originLatitude", "originLongitude"},
	telemetry.FieldDestLocation:     {"destinationLatitude", "destinationLongitude"},
	telemetry.FieldRouteLine:        {"navRouteCoordinates"},
}

// mapTelemetryToUpdate converts a map of telemetry field values into a
// VehicleUpdate with only the present fields set. Fields not recognized
// or missing from the map are left nil (no-op on the database update).
// Fields marked Invalid by the vehicle (e.g. cancelled navigation) are
// added to ClearFields so the database writer sets them to NULL.
func mapTelemetryToUpdate(fields map[string]events.TelemetryValue) *VehicleUpdate {
	u := &VehicleUpdate{}
	hasFields := false

	for name, val := range fields {
		// Nav fields marked invalid → schedule DB columns for NULL.
		if val.Invalid {
			if cols, isNav := navFieldColumns[telemetry.FieldName(name)]; isNav {
				u.ClearFields = append(u.ClearFields, cols...)
				hasFields = true
			}
			continue
		}

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

// applyFloat returns an applier that assigns a float64 value to the field
// returned by the target function.
func applyFloat(target func(u *VehicleUpdate) **float64) fieldApplier {
	return func(u *VehicleUpdate, val events.TelemetryValue) bool {
		if val.FloatVal == nil {
			return false
		}
		*target(u) = val.FloatVal
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
// DestinationLongitude fields. Zero-zero coordinates (protobuf default
// for "not set") are ignored to prevent overwriting real values.
func applyDestLocation(u *VehicleUpdate, val events.TelemetryValue) bool {
	if val.LocationVal == nil {
		return false
	}
	if val.LocationVal.Latitude == 0 && val.LocationVal.Longitude == 0 {
		return false
	}
	u.DestinationLatitude = &val.LocationVal.Latitude
	u.DestinationLongitude = &val.LocationVal.Longitude
	return true
}

// applyOriginLocation applies a LocationVal to OriginLatitude and
// OriginLongitude fields. Zero-zero coordinates are ignored.
func applyOriginLocation(u *VehicleUpdate, val events.TelemetryValue) bool {
	if val.LocationVal == nil {
		return false
	}
	if val.LocationVal.Latitude == 0 && val.LocationVal.Longitude == 0 {
		return false
	}
	u.OriginLatitude = &val.LocationVal.Latitude
	u.OriginLongitude = &val.LocationVal.Longitude
	return true
}

// applyRouteLine decodes Tesla's Base64-encoded RouteLine into [lng, lat]
// coordinate pairs (Mapbox/GeoJSON order) and marshals them as JSON for
// persistence in the navRouteCoordinates DB column. Empty strings clear
// the column (navigation cancelled).
func applyRouteLine(u *VehicleUpdate, val events.TelemetryValue) bool {
	if val.StringVal == nil {
		return false
	}
	if *val.StringVal == "" {
		// Empty RouteLine = navigation cancelled — clear the DB column.
		u.ClearFields = append(u.ClearFields, "navRouteCoordinates")
		return true
	}
	coords, err := polyline.DecodeRouteLine(*val.StringVal)
	if err != nil {
		slog.Warn("applyRouteLine: decode failed, clearing navRouteCoordinates",
			slog.Any("error", err),
		)
		u.ClearFields = append(u.ClearFields, "navRouteCoordinates")
		return true
	}
	// Convert from [lat, lng] (Google) to [lng, lat] (Mapbox/GeoJSON).
	mapboxCoords := make([][]float64, len(coords))
	for i, c := range coords {
		mapboxCoords[i] = []float64{c[1], c[0]}
	}
	raw, err := json.Marshal(mapboxCoords)
	if err != nil {
		slog.Warn("applyRouteLine: JSON marshal failed", slog.Any("error", err))
		return false
	}
	u.NavRouteCoordinates = jsonRawPtr(raw)
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
