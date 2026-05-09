package store

// Vehicle queries. All column names use double-quoted camelCase to match
// the Prisma-generated PostgreSQL schema.
//
// vehicleSelectColumns lists every column read into the Vehicle struct
// (and the encrypted-shadow ciphertext columns the read path needs to
// resolve a GPS pair or nav-route blob). The trailing six *Enc GPS
// columns are MYR-63 Phase 2 additions; the navRouteCoordinatesEnc
// column is the MYR-64 Phase 2 addition. See vehicle_gps_encryption.go
// and vehicle_repo_scan.go for the read-side preference rules.

const vehicleSelectColumns = `"id", "userId", "vin", "name",
	"model", "year", "color", "status",
	"chargeLevel", "estimatedRange", "chargeState", "timeToFull",
	"speed", "gearPosition", "heading",
	"latitude", "longitude", "locationName", "locationAddress",
	"interiorTemp", "exteriorTemp",
	"odometerMiles", "fsdMilesSinceReset",
	"destinationName", "destinationAddress", "destinationLatitude",
	"destinationLongitude", "originLatitude", "originLongitude",
	"etaMinutes", "tripDistanceRemaining",
	"navRouteCoordinates", "lastUpdated",
	"latitudeEnc", "longitudeEnc",
	"destinationLatitudeEnc", "destinationLongitudeEnc",
	"originLatitudeEnc", "originLongitudeEnc",
	"navRouteCoordinatesEnc"`

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

// Drive queries. The Drive table is Prisma-owned. The MYR-64 dual-write
// adds `routePointsEnc` (Text?) alongside the existing `routePoints`
// JSONB column. Append-on-write is plaintext-first via jsonb concat
// (||); the helper re-encrypts the post-append array into the shadow
// in a follow-up UPDATE so the plaintext path is never blocked on
// encryption.

const queryDriveInsert = `INSERT INTO "Drive" (
	"id", "vehicleId", "date", "startTime", "endTime",
	"startLocation", "startAddress", "endLocation", "endAddress",
	"distanceMiles", "durationMinutes", "avgSpeedMph", "maxSpeedMph",
	"energyUsedKwh", "startChargeLevel", "endChargeLevel",
	"fsdMiles", "fsdPercentage", "interventions", "routePoints",
	"routePointsEnc"
) VALUES (
	$1, $2, $3, $4, $5,
	$6, $7, $8, $9,
	$10, $11, $12, $13,
	$14, $15, $16,
	$17, $18, $19, $20::jsonb,
	$21
)`

// queryDriveAppendRoutePoints appends the delta to plaintext jsonb AND
// returns the post-append array so the caller can re-encrypt the full
// shape into the *Enc shadow in one round-trip. Phase 2 dual-write
// semantics: the plaintext column is the source-of-truth (Tesla
// telemetry never goes missing), so a shadow re-encrypt failure logs
// + fails open without rolling back the plaintext append.
const queryDriveAppendRoutePoints = `UPDATE "Drive"
SET "routePoints" = "routePoints" || $2::jsonb
WHERE "id" = $1
RETURNING "routePoints"`

// queryDriveSetRoutePointsEnc updates only the encrypted shadow. Used
// after queryDriveAppendRoutePoints succeeds so the plaintext write is
// not entangled with the encrypt round-trip.
const queryDriveSetRoutePointsEnc = `UPDATE "Drive"
SET "routePointsEnc" = $2
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
	"fsdMiles", "fsdPercentage", "interventions", "routePoints", "createdAt",
	"routePointsEnc"
FROM "Drive"
WHERE "id" = $1`

// Account queries. The Account table is Prisma-owned (NextAuth). We read
// tokens and update them in-place when refreshing expired OAuth tokens.
//
// During the MYR-62 cross-repo encryption rollout we live in a dual-write
// regime: the read path prefers `*_enc` (AES-256-GCM ciphertext) when
// non-NULL and falls back to the plaintext columns; the write path
// updates BOTH the plaintext column and the `*_enc` ciphertext column in
// one statement. The plaintext columns will be dropped in a separate
// post-rollout migration once every row is encrypted and the
// account_token_plaintext_remaining_total gauge reaches zero across all
// three columns. See docs/contracts/data-classification.md §3.3.

// #nosec G101 -- column-name SQL, not a credential. gosec greps the
// constant for "access_token" / "refresh_token" / "id_token" and flags
// the literal as a hardcoded credential by mistake.
const queryTeslaToken = `SELECT
    "access_token", "access_token_enc",
    "refresh_token", "refresh_token_enc",
    "id_token", "id_token_enc",
    "expires_at"
FROM "Account"
WHERE "userId" = $1 AND "provider" = 'tesla'
LIMIT 1`

const queryUpdateTeslaToken = `UPDATE "Account"
SET "access_token" = $1, "access_token_enc" = $2,
    "refresh_token" = $3, "refresh_token_enc" = $4,
    "expires_at" = $5
WHERE "userId" = $6 AND "provider" = 'tesla'`
