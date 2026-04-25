package store

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Vehicle queries. All column names use double-quoted camelCase to match
// the Prisma-generated PostgreSQL schema.

const vehicleSelectColumns = `"id", "userId", "vin", "name",
	"model", "year", "color", "status",
	"chargeLevel", "estimatedRange", "speed", "gearPosition", "heading",
	"latitude", "longitude", "locationName", "locationAddress",
	"interiorTemp", "exteriorTemp",
	"odometerMiles", "fsdMilesSinceReset",
	"destinationName", "destinationAddress", "destinationLatitude",
	"destinationLongitude", "originLatitude", "originLongitude",
	"etaMinutes", "tripDistanceRemaining",
	"navRouteCoordinates", "lastUpdated"`

const queryVehicleByVIN = `SELECT ` + vehicleSelectColumns + `
FROM "Vehicle"
WHERE "vin" = $1`

const queryVehicleByID = `SELECT ` + vehicleSelectColumns + `
FROM "Vehicle"
WHERE "id" = $1`

const queryVehiclesByUser = `SELECT ` + vehicleSelectColumns + `
FROM "Vehicle"
WHERE "userId" = $1
ORDER BY "name", "vin"`

// queryVehicleIDsByVIN is the slim companion of queryVehicleByVIN. It
// returns only the immutable identifiers (id, userId) so hot paths that
// need to map VIN → vehicleID/userID don't pull the heavy navRouteCoordinates
// JSON and other telemetry columns on every call.
const queryVehicleIDsByVIN = `SELECT "id", "userId" FROM "Vehicle" WHERE "vin" = $1`

const queryUpdateVehicleStatus = `UPDATE "Vehicle"
SET "status" = $1::"VehicleStatus", "lastUpdated" = NOW()
WHERE "vin" = $2`

// Drive queries.

const queryDriveInsert = `INSERT INTO "Drive" (
	"id", "vehicleId", "date", "startTime", "endTime",
	"startLocation", "startAddress", "endLocation", "endAddress",
	"distanceMiles", "durationMinutes", "avgSpeedMph", "maxSpeedMph",
	"energyUsedKwh", "startChargeLevel", "endChargeLevel",
	"fsdMiles", "fsdPercentage", "interventions", "routePoints"
) VALUES (
	$1, $2, $3, $4, $5,
	$6, $7, $8, $9,
	$10, $11, $12, $13,
	$14, $15, $16,
	$17, $18, $19, $20::jsonb
)`

const queryDriveAppendRoutePoints = `UPDATE "Drive"
SET "routePoints" = "routePoints" || $2::jsonb
WHERE "id" = $1`

const queryDriveComplete = `UPDATE "Drive"
SET "endTime" = $2, "endLocation" = $3, "endAddress" = $4,
	"distanceMiles" = $5, "durationMinutes" = $6,
	"avgSpeedMph" = $7, "maxSpeedMph" = $8, "energyUsedKwh" = $9,
	"endChargeLevel" = $10, "fsdMiles" = $11, "fsdPercentage" = $12,
	"interventions" = $13
WHERE "id" = $1`

const queryDriveByID = `SELECT "id", "vehicleId", "date", "startTime", "endTime",
	"startLocation", "startAddress", "endLocation", "endAddress",
	"distanceMiles", "durationMinutes", "avgSpeedMph", "maxSpeedMph",
	"energyUsedKwh", "startChargeLevel", "endChargeLevel",
	"fsdMiles", "fsdPercentage", "interventions", "routePoints", "createdAt"
FROM "Drive"
WHERE "id" = $1`

// Account queries. The Account table is Prisma-owned (NextAuth). We read
// tokens and update them in-place when refreshing expired OAuth tokens.

const queryTeslaToken = `SELECT "access_token", "refresh_token", "expires_at"
FROM "Account"
WHERE "userId" = $1 AND "provider" = 'tesla'
LIMIT 1`

const queryUpdateTeslaToken = `UPDATE "Account"
SET "access_token" = $1, "refresh_token" = $2, "expires_at" = $3
WHERE "userId" = $4 AND "provider" = 'tesla'`

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
		{"gearPosition", derefString(u.GearPosition), ""},
		{"heading", derefInt(u.Heading), ""},
		{"latitude", derefFloat(u.Latitude), ""},
		{"longitude", derefFloat(u.Longitude), ""},
		{"interiorTemp", derefInt(u.InteriorTemp), ""},
		{"exteriorTemp", derefInt(u.ExteriorTemp), ""},
		{"odometerMiles", derefInt(u.OdometerMiles), ""},
		{"locationName", derefString(u.LocationName), ""},
		{"locationAddress", derefString(u.LocationAddr), ""},
		{"destinationName", derefString(u.DestinationName), ""},
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
func buildTelemetryUpdate(vin string, u VehicleUpdate) (query string, args []any, ok bool) {
	var setClauses []string
	argIdx := 1

	// Build a set of columns to clear so we can skip them in the regular loop.
	clearSet := make(map[string]bool, len(u.ClearFields))
	for _, col := range u.ClearFields {
		clearSet[col] = true
	}

	for _, col := range updateColumns(u) {
		if col.val == nil || clearSet[col.col] {
			continue // skip nil values AND columns being explicitly cleared
		}
		// %q produces Go double-quoted strings which match PostgreSQL's
		// double-quoted identifier syntax. Column names are hardcoded constants.
		setClauses = append(setClauses, fmt.Sprintf("%q = $%d%s", col.col, argIdx, col.cast))
		args = append(args, col.val)
		argIdx++
	}

	// ClearFields: explicitly SET NULL for columns that should be cleared
	// (e.g. navigation cancelled by the vehicle).
	for _, col := range u.ClearFields {
		setClauses = append(setClauses, fmt.Sprintf("%q = NULL", col))
	}

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
