package drives

// Split out of detector.go to keep that file under the 300-line cap
// (CLAUDE.md "File Rules"). These helpers translate the loosely-typed
// telemetry payload (events.TelemetryValue, where every field is a
// nullable string/float/location wrapper) into the typed values the
// state machine reasons about. No state, no I/O — pure extractors.

import (
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// extractStringField returns the string value for a telemetry field,
// or empty string if absent or not a string.
func extractStringField(fields map[string]events.TelemetryValue, name telemetry.FieldName) string {
	v, ok := fields[string(name)]
	if !ok || v.StringVal == nil {
		return ""
	}
	return *v.StringVal
}

// extractFloatField returns the float64 value for a telemetry field,
// or 0 if absent or not a float.
func extractFloatField(fields map[string]events.TelemetryValue, name telemetry.FieldName) (float64, bool) {
	v, ok := fields[string(name)]
	if !ok || v.FloatVal == nil {
		return 0, false
	}
	return *v.FloatVal, true
}

// extractLocation returns the Location from the telemetry fields, or nil
// if absent.
func extractLocation(fields map[string]events.TelemetryValue) *events.Location {
	v, ok := fields[string(telemetry.FieldLocation)]
	if !ok || v.LocationVal == nil {
		return nil
	}
	return v.LocationVal
}
