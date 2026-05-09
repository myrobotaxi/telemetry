// VehicleUpdate → SQL helpers split out of queries.go to keep both
// files under the 300-line cap. The dynamic UPDATE builder is the only
// non-trivial logic in the store SQL layer; everything else is a
// constant query string.

package store

import (
	"encoding/json"
	"fmt"
	"strings"
)

// updateColumn pairs a PostgreSQL column name with the value to set. A nil
// value signals that the field was not present in this telemetry event.
type updateColumn struct {
	col  string
	val  any    // nil when the field pointer is nil
	cast string // optional PostgreSQL type cast (e.g. "::jsonb")
}

// updateColumns returns the list of column/value pairs for a VehicleUpdate.
// Values are dereferenced so callers can check for nil uniformly.
func updateColumns(u VehicleUpdate) []updateColumn {
	return []updateColumn{
		{"speed", derefInt(u.Speed), ""},
		{"chargeLevel", derefInt(u.ChargeLevel), ""},
		{"estimatedRange", derefInt(u.EstimatedRange), ""},
		{"chargeState", derefString(u.ChargeState), ""},
		{"timeToFull", derefFloat(u.TimeToFull), ""},
		{"gearPosition", derefString(u.GearPosition), ""},
		{"heading", derefInt(u.Heading), ""},
		{"latitude", derefFloat(u.Latitude), ""},
		{"longitude", derefFloat(u.Longitude), ""},
		{"interiorTemp", derefInt(u.InteriorTemp), ""},
		{"exteriorTemp", derefInt(u.ExteriorTemp), ""},
		{"odometerMiles", derefInt(u.OdometerMiles), ""},
		{"fsdMilesSinceReset", derefFloat(u.FsdMilesSinceReset), ""},
		{"locationName", derefString(u.LocationName), ""},
		{"locationAddress", derefString(u.LocationAddr), ""},
		{"destinationName", derefString(u.DestinationName), ""},
		{"destinationAddress", derefString(u.DestinationAddress), ""},
		{"destinationLatitude", derefFloat(u.DestinationLatitude), ""},
		{"destinationLongitude", derefFloat(u.DestinationLongitude), ""},
		{"originLatitude", derefFloat(u.OriginLatitude), ""},
		{"originLongitude", derefFloat(u.OriginLongitude), ""},
		{"etaMinutes", derefInt(u.EtaMinutes), ""},
		{"tripDistanceRemaining", derefFloat(u.TripDistRemaining), ""},
		{"navRouteCoordinates", derefJSON(u.NavRouteCoordinates), "::jsonb"},
	}
}

// deref helpers convert typed pointers to any, returning nil when the
// pointer is nil so the caller can skip the column.
func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefJSON(p *json.RawMessage) any {
	if p == nil {
		return nil
	}
	return *p
}

// buildTelemetryUpdate constructs a dynamic UPDATE statement for
// VehicleUpdate, including only columns whose values are non-nil.
// Returns the query string, the argument slice, and whether any fields
// were set. The caller should skip the UPDATE when ok is false.
//
// encShadows holds pre-computed *Enc ciphertexts keyed by *Enc column
// name (e.g. "latitudeEnc", "navRouteCoordinatesEnc"). Atomic-pair
// invariant for GPS pairs: callers MUST only insert keys in
// pair-complete pairs (both halves or neither) — see
// buildEncryptedGPSPair. nil/empty map disables the dual-write
// entirely (useful in tests that don't inject an Encryptor).
func buildTelemetryUpdate(vin string, u VehicleUpdate, encShadows map[string]string) (query string, args []any, ok bool) {
	clearSet := buildClearSet(u.ClearFields)

	var setClauses []string
	argIdx := 1

	setClauses, args, argIdx = appendPlaintextSets(setClauses, args, argIdx, u, clearSet)
	setClauses, args, argIdx = appendGPSShadowSets(setClauses, args, argIdx, encShadows, clearSet)
	setClauses, args, argIdx = appendNavRouteShadowSet(setClauses, args, argIdx, encShadows, clearSet)
	setClauses = appendClearFieldSets(setClauses, u.ClearFields)

	if len(setClauses) == 0 {
		return "", nil, false
	}

	// Always update lastUpdated.
	setClauses = append(setClauses, fmt.Sprintf(`"lastUpdated" = $%d`, argIdx))
	args = append(args, u.LastUpdated)
	argIdx++

	// VIN is the final parameter for the WHERE clause.
	args = append(args, vin)
	query = fmt.Sprintf(`UPDATE "Vehicle" SET %s WHERE "vin" = $%d`,
		strings.Join(setClauses, ", "), argIdx)
	return query, args, true
}

// buildClearSet flips the ClearFields slice into a set so the inner
// loops can short-circuit a column being explicitly cleared.
func buildClearSet(clearFields []string) map[string]bool {
	clearSet := make(map[string]bool, len(clearFields))
	for _, col := range clearFields {
		clearSet[col] = true
	}
	return clearSet
}

// appendPlaintextSets walks every non-nil VehicleUpdate field and
// appends the corresponding `<col> = $N[::cast]` clause + argument.
// Columns being explicitly cleared are skipped (handled later by
// appendClearFieldSets).
func appendPlaintextSets(setClauses []string, args []any, argIdx int, u VehicleUpdate, clearSet map[string]bool) ([]string, []any, int) {
	for _, col := range updateColumns(u) {
		if col.val == nil || clearSet[col.col] {
			continue
		}
		// %q produces Go double-quoted strings which match PostgreSQL's
		// double-quoted identifier syntax. Column names are constants.
		setClauses = append(setClauses, fmt.Sprintf("%q = $%d%s", col.col, argIdx, col.cast))
		args = append(args, col.val)
		argIdx++
	}
	return setClauses, args, argIdx
}

// appendGPSShadowSets emits the MYR-63 GPS *Enc shadows in canonical
// pair order. Half-pair entries are skipped defensively — the caller's
// atomic-pair guard should reject them, but a half-pair UPDATE would
// trip the read-side fallback, so we filter again.
func appendGPSShadowSets(setClauses []string, args []any, argIdx int, encShadows map[string]string, clearSet map[string]bool) ([]string, []any, int) {
	for _, p := range gpsPairs {
		latEncCol := p.lat + "Enc"
		lngEncCol := p.lng + "Enc"
		latCT, hasLat := encShadows[latEncCol]
		lngCT, hasLng := encShadows[lngEncCol]
		if !hasLat || !hasLng {
			continue
		}
		if clearSet[p.lat] || clearSet[p.lng] {
			continue
		}
		setClauses = append(setClauses, fmt.Sprintf("%q = $%d", latEncCol, argIdx))
		args = append(args, latCT)
		argIdx++
		setClauses = append(setClauses, fmt.Sprintf("%q = $%d", lngEncCol, argIdx))
		args = append(args, lngCT)
		argIdx++
	}
	return setClauses, args, argIdx
}

// appendNavRouteShadowSet emits the MYR-64 nav-route blob shadow when
// the encrypted ciphertext is present and the plaintext column isn't
// being concurrently cleared.
func appendNavRouteShadowSet(setClauses []string, args []any, argIdx int, encShadows map[string]string, clearSet map[string]bool) ([]string, []any, int) {
	navCT, ok := encShadows["navRouteCoordinatesEnc"]
	if !ok || clearSet["navRouteCoordinates"] {
		return setClauses, args, argIdx
	}
	setClauses = append(setClauses, fmt.Sprintf("%q = $%d", "navRouteCoordinatesEnc", argIdx))
	args = append(args, navCT)
	argIdx++
	return setClauses, args, argIdx
}

// appendClearFieldSets emits an explicit `SET NULL` for every
// ClearFields entry, plus the matching *Enc shadow when the column has
// one. This keeps the dual-write invariant intact through navigation
// cancellation: a NULL plaintext + stale ciphertext would surface as a
// corrupt half-pair on read.
func appendClearFieldSets(setClauses []string, clearFields []string) []string {
	for _, col := range clearFields {
		setClauses = append(setClauses, fmt.Sprintf("%q = NULL", col))
		if encCol, ok := plaintextToEncColumn[col]; ok {
			setClauses = append(setClauses, fmt.Sprintf("%q = NULL", encCol))
		}
	}
	return setClauses
}

// plaintextToEncColumn maps each plaintext column to its *Enc
// shadow. Used by buildTelemetryUpdate to extend ClearFields-driven
// `SET NULL` to the encrypted shadow so a navigation-cancelled row
// doesn't end up with a NULL plaintext + stale ciphertext (the same
// half-pair corruption mode the read path warns about).
//
// MYR-64 adds the navRouteCoordinates → navRouteCoordinatesEnc pair so
// a "navigation cancelled" event clears the route blob shadow alongside
// its plaintext column.
var plaintextToEncColumn = map[string]string{
	"latitude":             "latitudeEnc",
	"longitude":            "longitudeEnc",
	"destinationLatitude":  "destinationLatitudeEnc",
	"destinationLongitude": "destinationLongitudeEnc",
	"originLatitude":       "originLatitudeEnc",
	"originLongitude":      "originLongitudeEnc",
	"navRouteCoordinates":  "navRouteCoordinatesEnc",
}
