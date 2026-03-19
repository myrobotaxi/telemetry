package ws

import "github.com/tnando/my-robo-taxi-telemetry/internal/events"

// internalToClientField maps internal telemetry field names (from the
// telemetry package) to the frontend Vehicle model field names expected by
// the MyRoboTaxi Next.js app. Fields not in this map are passed through
// with their internal name.
var internalToClientField = map[string]string{
	"soc":         "chargeLevel",
	"gear":        "gearPosition",
	"odometer":    "odometerMiles",
	"insideTemp":  "interiorTemp",
	"outsideTemp": "exteriorTemp",
	// These fields map 1:1 and are listed for explicitness:
	// speed, heading, estimatedRange, location (handled separately)
}

// mapFieldsForClient converts a map of internal TelemetryValue fields into
// plain key-value pairs suitable for JSON serialization to browser clients.
// Pointer-wrapped values are unwrapped, LocationVal is split into separate
// "latitude" and "longitude" fields, and field names are translated to
// match the frontend Vehicle model.
func mapFieldsForClient(fields map[string]events.TelemetryValue) map[string]any {
	out := make(map[string]any, len(fields))
	for name, val := range fields {
		if name == "location" {
			if val.LocationVal != nil {
				out["latitude"] = val.LocationVal.Latitude
				out["longitude"] = val.LocationVal.Longitude
			}
			continue
		}

		clientName := translateFieldName(name)
		if v := unwrapValue(val); v != nil {
			out[clientName] = v
		}
	}
	return out
}

// translateFieldName returns the frontend field name for an internal field
// name. If no mapping exists, the internal name is returned unchanged.
func translateFieldName(internal string) string {
	if client, ok := internalToClientField[internal]; ok {
		return client
	}
	return internal
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
