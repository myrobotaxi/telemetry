package store

import (
	"fmt"
	"strings"
)

// Vehicle queries. All column names use double-quoted camelCase to match
// the Prisma-generated PostgreSQL schema.

const vehicleSelectColumns = `"id", "userId", "vin", "name", "status",
	"chargeLevel", "estimatedRange", "speed", "gearPosition", "heading",
	"latitude", "longitude", "interiorTemp", "exteriorTemp",
	"odometerMiles", "lastUpdated"`

const queryVehicleByVIN = `SELECT ` + vehicleSelectColumns + `
FROM "Vehicle"
WHERE "vin" = $1`

const queryVehicleByID = `SELECT ` + vehicleSelectColumns + `
FROM "Vehicle"
WHERE "id" = $1`

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

// buildTelemetryUpdate constructs a dynamic UPDATE statement for
// VehicleUpdate, including only columns whose values are non-nil.
// Returns the query string, the argument slice, and whether any fields
// were set. The caller should skip the UPDATE when ok is false.
func buildTelemetryUpdate(vin string, u VehicleUpdate) (query string, args []any, ok bool) {
	var setClauses []string
	argIdx := 1

	// %q produces Go double-quoted strings which match PostgreSQL's
	// double-quoted identifier syntax. Column names are hardcoded constants.
	addClause := func(col string, val any) {
		setClauses = append(setClauses, fmt.Sprintf("%q = $%d", col, argIdx))
		args = append(args, val)
		argIdx++
	}

	if u.Speed != nil {
		addClause("speed", *u.Speed)
	}
	if u.ChargeLevel != nil {
		addClause("chargeLevel", *u.ChargeLevel)
	}
	if u.EstimatedRange != nil {
		addClause("estimatedRange", *u.EstimatedRange)
	}
	if u.GearPosition != nil {
		addClause("gearPosition", *u.GearPosition)
	}
	if u.Heading != nil {
		addClause("heading", *u.Heading)
	}
	if u.Latitude != nil {
		addClause("latitude", *u.Latitude)
	}
	if u.Longitude != nil {
		addClause("longitude", *u.Longitude)
	}
	if u.InteriorTemp != nil {
		addClause("interiorTemp", *u.InteriorTemp)
	}
	if u.ExteriorTemp != nil {
		addClause("exteriorTemp", *u.ExteriorTemp)
	}
	if u.OdometerMiles != nil {
		addClause("odometerMiles", *u.OdometerMiles)
	}
	if u.LocationName != nil {
		addClause("locationName", *u.LocationName)
	}
	if u.LocationAddr != nil {
		addClause("locationAddress", *u.LocationAddr)
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
