package store

import (
	"encoding/json"
	"log/slog"
	"math"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/polyline"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// fieldApplier applies a TelemetryValue to the matching field on a
// VehicleUpdate. Returns true if the value was applied.
type fieldApplier func(u *VehicleUpdate, val events.TelemetryValue) bool

// fieldAppliers maps each tracked telemetry field to its applier function.
var fieldAppliers = map[telemetry.FieldName]fieldApplier{
	telemetry.FieldSpeed:            applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.Speed }),
	telemetry.FieldHeading:          applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.Heading }),
	telemetry.FieldSOC:              applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.ChargeLevel }),
	telemetry.FieldBatteryLevel:     applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.ChargeLevel }),
	telemetry.FieldEstBatteryRange:  applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.EstimatedRange }),
	telemetry.FieldChargeState:      applyString(func(u *VehicleUpdate) **string { return &u.ChargeState }),
	telemetry.FieldTimeToFull:       applyFloat(func(u *VehicleUpdate) **float64 { return &u.TimeToFull }),
	telemetry.FieldInsideTemp:       applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.InteriorTemp }),
	telemetry.FieldOutsideTemp:      applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.ExteriorTemp }),
	telemetry.FieldOdometer:         applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.OdometerMiles }),
	telemetry.FieldFSDMiles:         applyFloat(func(u *VehicleUpdate) **float64 { return &u.FsdMilesSinceReset }),
	telemetry.FieldGear:             applyString(func(u *VehicleUpdate) **string { return &u.GearPosition }),
	telemetry.FieldLocation:         applyLocation,
	telemetry.FieldDestinationName:  applyString(func(u *VehicleUpdate) **string { return &u.DestinationName }),
	telemetry.FieldDestLocation:     applyDestLocation,
	telemetry.FieldOriginLocation:   applyOriginLocation,
	telemetry.FieldMinutesToArrival: applyFloatAsInt(func(u *VehicleUpdate) **int { return &u.EtaMinutes }),
	telemetry.FieldMilesToArrival:   applyFloat(func(u *VehicleUpdate) **float64 { return &u.TripDistRemaining }),
	telemetry.FieldRouteLine:        applyRouteLine,
}

// navFieldColumns maps internal telemetry field names to the DB column
// names that should be SET NULL when the vehicle marks the field invalid
// (e.g. navigation cancelled).
var navFieldColumns = map[telemetry.FieldName][]string{
	telemetry.FieldDestinationName:  {"destinationName"},
	telemetry.FieldMinutesToArrival: {"etaMinutes"},
	telemetry.FieldMilesToArrival:   {"tripDistanceRemaining"},
	telemetry.FieldOriginLocation:   {"originLatitude", "originLongitude"},
	telemetry.FieldDestLocation:     {"destinationLatitude", "destinationLongitude", "destinationAddress"},
	telemetry.FieldRouteLine:        {"navRouteCoordinates"},
}

// navClearColumns is the set of DB columns whose appearance in ClearFields
// means the CAR reported navigation invalid — a cancel. Derived from
// navFieldColumns rather than restated, so the two cannot drift: a nav field
// that gains a column there gains it here.
var navClearColumns = buildNavClearColumns()

func buildNavClearColumns() map[string]struct{} {
	out := make(map[string]struct{})
	for _, cols := range navFieldColumns {
		for _, col := range cols {
			out[col] = struct{}{}
		}
	}
	return out
}

// carriedNavReading reports whether this update was built from a frame that
// actually told us something about NAVIGATION (MYR-409).
//
// It is asked of the finished update rather than of the raw field map on
// purpose: the answer is then "a nav reading we acted on", not "a key was
// present". A nav field the applier rejected sets no pointer and schedules no
// clear, so it stamps nothing — which is right, since nothing about the stored
// navigation changed.
//
// BOTH ARMS COUNT. A frame that carries a destination, an ETA, a remaining
// distance, an origin or a route line is the obvious case. A frame that CLEARS
// them is equally a reading: "this car has no route" is current information
// about navigation, and the same update NULLs the values, so the stamp can
// never end up dating a value that is no longer in the row.
//
// DestinationAddress is deliberately excluded from the pointer arm. It is the
// reverse-geocoded rendering of a destination the server itself resolves
// (applyDestinationAddress, on the flush path), not something the car said, so
// letting it stamp would date a nav reading by our own geocoder's clock. Its
// COLUMN still counts in the clear arm, because a `destinationAddress` entry in
// ClearFields can only have come from the car invalidating DestLocation.
func (u *VehicleUpdate) carriedNavReading() bool {
	if u.DestinationName != nil ||
		u.DestinationLatitude != nil || u.DestinationLongitude != nil ||
		u.OriginLatitude != nil || u.OriginLongitude != nil ||
		u.EtaMinutes != nil || u.TripDistRemaining != nil ||
		u.NavRouteCoordinates != nil {
		return true
	}
	for _, col := range u.ClearFields {
		if _, isNav := navClearColumns[col]; isNav {
			return true
		}
	}
	return false
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

	// MYR-269: derive the owner-control side-table fields (lock, trunk/frunk,
	// climate, charge-port) from the same field map. These land in the Go-owned
	// go_vehicle_control_state table, not the Vehicle table, so they are carried
	// on VehicleUpdate.ControlState and upserted separately by the writer flush.
	// Their presence also counts as "has fields" so a control-only frame (e.g. a
	// lone lock toggle, or the MYR-260 /vehicle_data backfill) is not dropped.
	if cs := mapTelemetryToControlState(fields); cs != nil {
		u.ControlState = cs
		hasFields = true
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
