package ws

import (
	"log/slog"
	"math"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

// internalToClientField maps internal telemetry field names (from the
// telemetry package) to the frontend Vehicle model field names expected by
// the MyRoboTaxi Next.js app. Fields not in this map are passed through
// with their internal name.
var internalToClientField = map[string]string{
	"soc":                    "chargeLevel",
	"gear":                   "gearPosition",
	"odometer":               "odometerMiles",
	"insideTemp":             "interiorTemp",
	"outsideTemp":            "exteriorTemp",
	"minutesToArrival":       "etaMinutes",
	"milesToArrival":         "tripDistanceRemaining",
	"fsdMilesSinceReset":     "fsdMilesToday",
	"hvacFanSpeed":           "fanSpeed",
	// These fields map 1:1 and are listed for explicitness:
	// speed, heading, estimatedRange, location (handled separately)
	// hvacPower, defrostMode, climateKeeperMode, driverTempSetting,
	// passengerTempSetting, seatHeaterLeft, seatHeaterRight pass through unchanged.
}

// integerFields are client field names that the frontend Vehicle model types
// as integers. Float values for these fields are rounded before serialization.
var integerFields = map[string]struct{}{
	"speed":                {},
	"heading":              {},
	"chargeLevel":          {},
	"estimatedRange":       {},
	"etaMinutes":           {},
	"interiorTemp":         {},
	"exteriorTemp":         {},
	"odometerMiles":        {},
	"fanSpeed":             {},
	"seatHeaterLeft":       {},
	"seatHeaterRight":      {},
	"driverTempSetting":    {},
	"passengerTempSetting": {},
}

// locationFieldSplit maps internal location field names to the pair of
// client field names they should be split into (latitude, longitude).
var locationFieldSplit = map[string][2]string{
	"location":            {"latitude", "longitude"},
	"destinationLocation": {"destinationLatitude", "destinationLongitude"},
	"originLocation":      {"originLatitude", "originLongitude"},
}

// mapFieldsForClient converts a map of internal TelemetryValue fields into
// plain key-value pairs suitable for JSON serialization to browser clients.
// Pointer-wrapped values are unwrapped, LocationVal fields are split into
// separate latitude/longitude pairs, routeLine is decoded into coordinates,
// field names are translated to match the frontend Vehicle model, and
// isClimateOn is derived from hvacPower when that field is present.
func mapFieldsForClient(fields map[string]events.TelemetryValue) map[string]any {
	out := make(map[string]any, len(fields))
	for name, val := range fields {
		switch {
		case locationFieldSplit[name] != [2]string{}:
			splitLocationField(out, name, val)
		case name == "routeLine":
			decodeRouteLineField(out, val)
		default:
			clientName := translateFieldName(name)
			if v := unwrapValue(val); v != nil {
				out[clientName] = roundIfInteger(clientName, v)
			}
		}
	}
	// Derive isClimateOn from hvacPower when present so the frontend can
	// render the climate card without needing to interpret the enum itself.
	if power, ok := out["hvacPower"].(string); ok {
		out["isClimateOn"] = power != "off"
	}
	return out
}

// splitLocationField adds a LocationVal as separate latitude/longitude keys
// to the output map. If the LocationVal is nil, no keys are added.
// For origin and destination locations, zero-zero coordinates (protobuf
// default for "not set") are skipped to prevent overwriting real values.
func splitLocationField(out map[string]any, name string, val events.TelemetryValue) {
	if val.LocationVal == nil {
		return
	}
	// Skip zero-zero for origin/destination — protobuf default means "not set".
	// Vehicle Location is not skipped because it updates frequently and 0,0 is
	// filtered upstream by the minimum_delta config.
	if name != "location" &&
		val.LocationVal.Latitude == 0 && val.LocationVal.Longitude == 0 {
		return
	}
	latLng := locationFieldSplit[name]
	out[latLng[0]] = val.LocationVal.Latitude
	out[latLng[1]] = val.LocationVal.Longitude
}

// decodeRouteLineField decodes a Google Encoded Polyline string and adds
// the resulting coordinates as "navRouteCoordinates" in [lng, lat] (Mapbox)
// format. Empty or nil strings are silently skipped.
func decodeRouteLineField(out map[string]any, val events.TelemetryValue) {
	if val.StringVal == nil {
		slog.Warn("decodeRouteLineField: routeLine arrived but StringVal is nil, unexpected type")
		return
	}
	if *val.StringVal == "" {
		return
	}
	slog.Info("decodeRouteLineField: routeLine received",
		slog.Int("encoded_len", len(*val.StringVal)),
	)
	coords, err := DecodePolyline(*val.StringVal)
	if err != nil {
		slog.Warn("mapFieldsForClient: failed to decode routeLine",
			slog.Any("error", err),
		)
		return
	}
	// Convert from [lat, lng] (Google) to [lng, lat] (Mapbox/GeoJSON).
	mapboxCoords := make([][]float64, len(coords))
	for i, c := range coords {
		mapboxCoords[i] = []float64{c[1], c[0]}
	}
	out["navRouteCoordinates"] = mapboxCoords

	// Diagnostic: log first/last coords and count so we can verify the route
	if len(mapboxCoords) > 0 {
		first := mapboxCoords[0]
		last := mapboxCoords[len(mapboxCoords)-1]
		slog.Info("decodeRouteLineField: navRouteCoordinates set",
			slog.Int("points", len(mapboxCoords)),
			slog.Float64("first_lng", first[0]),
			slog.Float64("first_lat", first[1]),
			slog.Float64("last_lng", last[0]),
			slog.Float64("last_lat", last[1]),
		)
	}
}

// roundIfInteger rounds float64 values to integers for fields the frontend
// types as int. Non-float values and fields not in integerFields pass through.
func roundIfInteger(field string, v any) any {
	if _, ok := integerFields[field]; !ok {
		return v
	}
	if f, ok := v.(float64); ok {
		return int(math.Round(f))
	}
	return v
}

// translateFieldName returns the frontend field name for an internal field
// name. If no mapping exists, the internal name is returned unchanged.
func translateFieldName(internal string) string {
	if client, ok := internalToClientField[internal]; ok {
		return client
	}
	return internal
}

// deriveVehicleStatus infers the vehicle status from the mapped client fields.
// The frontend reads vehicle.status to decide which UI to render.
func deriveVehicleStatus(fields map[string]any) string {
	gear, _ := fields["gearPosition"].(string)

	var speed float64
	switch v := fields["speed"].(type) {
	case float64:
		speed = v
	case int:
		speed = float64(v)
	}

	switch {
	case gear == "D" || gear == "R" || speed > 0:
		return "driving"
	default:
		return "parked"
	}
}

// unwrapValue extracts the plain value from a TelemetryValue union. Returns
// nil if no field is set.
func unwrapValue(v events.TelemetryValue) any {
	switch {
	case v.FloatVal != nil:
		return *v.FloatVal
	case v.IntVal != nil:
		return *v.IntVal
	case v.StringVal != nil:
		return *v.StringVal
	case v.BoolVal != nil:
		return *v.BoolVal
	case v.LocationVal != nil:
		// Location is handled separately in mapFieldsForClient.
		return nil
	default:
		return nil
	}
}
