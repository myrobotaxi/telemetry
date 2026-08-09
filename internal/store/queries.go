package store

// Vehicle queries. All column names use double-quoted camelCase to match
// the Prisma-generated PostgreSQL schema.
//
// vehicleSelectColumns lists every column read into the Vehicle struct.
// Location data is read ONLY from the encrypted-shadow ciphertext
// columns: MYR-433 retired the plaintext GPS and nav-route columns from
// every read path, so the six `*Enc` GPS columns and
// `navRouteCoordinatesEnc` are the sole source of truth for coordinates.
//
// MYR-447 extended that rule to the human-readable rendering of the same
// position: "locationNameEnc", "locationAddressEnc",
// "destinationNameEnc" and "destinationAddressEnc" replaced their
// plaintext originals here. A street address needs no map to interpret,
// so leaving it readable while the coordinate beside it was sealed made
// the coordinate's encryption largely decorative.
//
// The plaintext columns ("latitude", "longitude", "destinationLatitude",
// "destinationLongitude", "originLatitude", "originLongitude",
// "navRouteCoordinates", "locationName", "locationAddress",
// "destinationName", "destinationAddress") still EXIST on the
// Prisma-owned table — they are dropped by a react-frontend Prisma
// migration, not from here (CG-DL-9 forbids Go migrations touching Prisma
// tables). They MUST NOT be added back to this projection: selecting them
// is what made an operator with DB access able to read a user's location
// (MYR-433 / MYR-447 acceptance bar). `cmd/purge-plaintext-columns`
// scrubs their residual values.
//
// See vehicle_gps_encryption.go, label_encryption.go and
// vehicle_repo_scan.go for the ciphertext-only read rules.

const vehicleSelectColumns = `"id", "userId", "vin", "name",
	"model", "year", "color", "licensePlate", "status",
	"chargeLevel", "estimatedRange", "chargeState", "timeToFull",
	"speed", "gearPosition", "heading",
	"locationNameEnc", "locationAddressEnc",
	"interiorTemp", "exteriorTemp",
	"odometerMiles", "fsdMilesSinceReset",
	"destinationNameEnc", "destinationAddressEnc",
	"etaMinutes", "tripDistanceRemaining",
	"lastUpdated",
	"latitudeEnc", "longitudeEnc",
	"destinationLatitudeEnc", "destinationLongitudeEnc",
	"originLatitudeEnc", "originLongitudeEnc",
	"navRouteCoordinatesEnc"`

const queryVehicleByVIN = `SELECT ` + vehicleSelectColumns + `
FROM "Vehicle"
WHERE "vin" = $1`

// queryVehicleByID is the snapshot (GET /snapshot) read path. It LEFT JOINs the
// Go-owned go_vehicle_control_state side table (MYR-269, MYR-273, MYR-279,
// MYR-274, MYR-298, MYR-303, MYR-308, MYR-316, MYR-320) so the response carries the durable
// owner-control read-backs for a
// non-streaming car: the five MYR-269 booleans (lock, trunk/frunk, climate,
// charge-port), the eleven MYR-273 cabin-setting levels (driver/passenger temp
// setpoints, fan speed, seat heater/cooler levels, media volume), the two MYR-279
// vehicle-detail strings (software version, trim), and the two MYR-274 climate-mode
// read-backs (hvac auto mode, A/C enabled), the two MYR-298 read-backs (seat
// ventilation, media playback status), the eight MYR-303 media now-playing
// columns, the MYR-308 ventilated-seat capability bit, the two MYR-316
// service-window timestamps, and the two MYR-320 detail strings (trim label,
// FSD version). The control columns
// are appended AFTER the
// *Enc columns so scanVehicleRowExtra reads them as trailing `extra` destinations.
// Joining a Go-owned table into a Prisma-owned read is a runtime SELECT, not a
// migration FK, so CG-DL-9 does not apply. The base columns stay unqualified
// (unambiguous — the side table's snake_case columns share no name with the
// quoted camelCase "Vehicle" columns); "id" is qualified to name the join key.
const queryVehicleByID = `SELECT ` + vehicleSelectColumns + `,
	gcs.is_locked, gcs.frunk_open, gcs.trunk_open, gcs.is_climate_on, gcs.charge_port_open,
	gcs.driver_temp_setting, gcs.passenger_temp_setting, gcs.fan_speed,
	gcs.seat_heater_left, gcs.seat_heater_right,
	gcs.seat_heater_rear_left, gcs.seat_heater_rear_center, gcs.seat_heater_rear_right,
	gcs.seat_cooler_left, gcs.seat_cooler_right, gcs.media_volume,
	gcs.software_version, gcs.trim,
	gcs.hvac_auto_mode, gcs.hvac_ac_enabled,
	gcs.seat_vent_enabled, gcs.media_playback_status,
	gcs.media_now_playing_title, gcs.media_now_playing_artist, gcs.media_now_playing_album,
	gcs.media_now_playing_station, gcs.media_playback_source,
	gcs.media_now_playing_duration_ms, gcs.media_now_playing_elapsed_ms, gcs.media_volume_max,
	gcs.seat_cooling_capable,
	gcs.service_etc, gcs.service_expected_end_at,
	gcs.trim_label, gcs.fsd_version,
	` + rideShareEnabledExpr + `,
	` + setupScheduleColumns + `
FROM "Vehicle"
LEFT JOIN go_vehicle_control_state gcs ON gcs.vehicle_id = "Vehicle"."id"` + setupScheduleJoin + `
WHERE "Vehicle"."id" = $1`

// rideShareEnabledExpr is the single definition of "does this car accept ride
// requests right now?" for every read that already LEFT JOINs the control-state
// side table as `gcs` (MYR-342): the snapshot read, the owner catalog list, and
// the viewer catalog list.
//
// The COALESCE is LOAD-BEARING and is why this is a shared constant rather than
// three inline copies. `ride_share_enabled` is NOT NULL (migration 0021), so the
// only way it can arrive NULL is the LEFT JOIN finding no side-table row at all
// — an ordinary state for a car that has never had a control write. That must be
// indistinguishable from an explicitly-enabled car, because the wire contract
// says an absent/unset switch means ENABLED. One reader spelling this
// differently would emit `false` for a car nobody has ever paused, i.e. would
// hide a perfectly available vehicle from its own owner.
//
// Costs nothing: the join is already there for the MYR-316 service window (list)
// and the whole control-state block (snapshot), so this rides an existing probe
// of the vehicle_id PRIMARY KEY and adds one fixed-width boolean.
const rideShareEnabledExpr = `COALESCE(gcs.ride_share_enabled, TRUE) AS "rideShareEnabled"`

const queryVehiclesByUser = `SELECT ` + vehicleSelectColumns + `
FROM "Vehicle"
WHERE "userId" = $1
ORDER BY "name", "vin"`

// vehicleListSummaryColumns is the lean projection used by the
// GET /api/vehicles list endpoint (MYR-122). It MUST stay aligned with
// the wire fields emitted by
// `internal/telemetry/vehicles_list_handler.go` `vehicleSummary`. No
// GPS columns (plaintext or `*Enc`), no `navRouteCoordinates`, no
// climate / odometer / fsd columns — list endpoints don't need them
// and pulling them on every row is what made `GET /api/vehicles` cost
// ~1.3 min on a warm DB.
//
// Anchored by AGENTS.md "Performance invariants": list endpoints use
// lean projections; wide selects belong only in detail/edit handlers.
const vehicleListSummaryColumns = `"id", "userId", "vin", "name",
	"model", "year", "color", "licensePlate", "status",
	"chargeLevel", "estimatedRange", "lastUpdated"`

// vehicleListHasActiveRideExpr derives the `hasActiveRide` catalog flag
// (MYR-233): TRUE iff the car holds an OPEN INSTANT ride request.
//
// The predicate is character-for-character the predicate of the partial
// unique index `uq_go_ride_requests_active_instant_vehicle` (migration
// 0013, MYR-266), which is the whole point:
//
//  1. The flag can NEVER disagree with the accept guard — flag true ⇒ an
//     accept would raise 23505 on that index, and vice versa.
//  2. Postgres answers the correlated EXISTS from the partial index
//     (verified: `Index Only Scan using
//     uq_go_ride_requests_active_instant_vehicle`), so the flag costs
//     one index probe per row — no extra round trip, no N+1. The index
//     also bounds the match at ONE row per vehicle.
//
// Excluded, mirroring the index: scheduled rides (a reservation never
// makes the car busy), `requested` (many riders may hold pending
// requests against one idle car), and the terminal states. A new
// committed lifecycle state must be added HERE and to 0013's index in
// the same PR or the flag drifts from the guard it mirrors.
const vehicleListHasActiveRideExpr = `EXISTS (
		SELECT 1
		FROM go_ride_requests r
		WHERE r.vehicle_id = "Vehicle"."id"
		  AND ` + activeInstantRidePredicate + `
	) AS "hasActiveRide"`

// activeInstantRidePredicate is the single definition of "this car is BUSY
// right now" — character-for-character the predicate of the partial unique
// index `uq_go_ride_requests_active_instant_vehicle` (migration 0013,
// MYR-266), written against an aliased `r` so every consumer shares it:
//
//   - vehicleListHasActiveRideExpr — the `hasActiveRide` catalog flag (MYR-233).
//   - queryRideRequestVehicleBusy — the reservation sweeper's vehicle-busy
//     hold (MYR-179): a reservation coming due must not dial nav at a car that
//     is mid-ride, so it holds and retries instead of claiming.
//
// Extracted (MYR-179) precisely so a THIRD reader cannot drift from the index
// it mirrors. Excluded, mirroring the index: SCHEDULED rides (a reservation
// never makes the car busy — which is also why one reservation coming due
// cannot block another for the same car; that is a v1 boundary, see
// rest-api.md §7.8), `requested` (many riders may hold pending requests
// against one idle car), and the terminal states. A new committed lifecycle
// state must be added HERE and to 0013's index in the same PR.
const activeInstantRidePredicate = `r.scheduled_for IS NULL
		  AND r.status IN ('accepted', 'enroute', 'arrived')`

// queryVehiclesByUserList is the lean read path for the catalog list
// endpoint. Companion to queryVehiclesByUser (wide read, kept for
// detail/edit consumers). ORDER BY matches the wide query so the SDK
// sees stable iteration order across both surfaces.
//
// MYR-233: the derived `hasActiveRide` flag rides along as a correlated
// EXISTS rather than a follow-up per-vehicle query — one statement
// serves the whole catalog, preserving the MYR-122 single-round-trip
// property of this path.
// MYR-316: the service-window pair is the FIRST side-table data this lean
// query carries, so it also introduces the LEFT JOIN. That is a deliberate,
// bounded departure from the MYR-122 rule of thumb ("no join on the list
// path"), and it stays inside the rule that actually matters — the invariant
// is "MUST NOT SELECT columns the response body doesn't emit", and both of
// these ARE emitted, as VehicleSummary.serviceEstimatedEndAt.
//
// The cost is one probe of go_vehicle_control_state's vehicle_id PRIMARY KEY
// per row — the same index the snapshot read already joins on — and two
// fixed-width timestamps. It pulls no JSON blob, no encrypted shadow, and no
// text column, which is what made the pre-MYR-122 wide read expensive. The
// alternative, an N+1 per-vehicle lookup from the handler, would be strictly
// worse on the very path MYR-122 exists to protect.
// MYR-342: the owner ride-sharing switch rides the SAME LEFT JOIN, so it costs
// nothing beyond one fixed-width boolean, and it IS emitted (as
// VehicleSummary.rideShareEnabled) — the invariant this projection actually
// enforces is "MUST NOT SELECT columns the response body doesn't emit".
// MYR-491: the setup schedule is the SECOND side-table join on this path, and
// it earns its place by the same invariant — all four values are emitted, as
// the single derived VehicleSummary.setupState. A per-vehicle follow-up read
// instead would be an N+1 on the catalog, and the rider-side picker is the
// consumer that most needs the value (MYR-437: "setting up", not "offline"), so
// it cannot be pushed onto the snapshot alone.
const queryVehiclesByUserList = `SELECT ` + vehicleListSummaryColumns + `,
	` + vehicleListHasActiveRideExpr + `,
	gcs.service_etc, gcs.service_expected_end_at,
	` + rideShareEnabledExpr + `,
	` + setupScheduleColumns + `
FROM "Vehicle"
LEFT JOIN go_vehicle_control_state gcs ON gcs.vehicle_id = "Vehicle"."id"` + setupScheduleJoin + `
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

// queryUpdateVehicleStatusBaseline writes a NON-MOTION baseline status only
// where it cannot contradict the stream (MYR-454).
//
// The ServiceStatusMonitor persists a "connected baseline" of 'parked' on
// every connectivity edge whose REST read says the car is not in service. That
// was harmless while 'driving' was unreachable in this column — 'parked' was
// the most specific value the column could hold. Now that the telemetry fold
// writes 'driving', an unconditional baseline write would stomp it on every
// reconnect and reproduce the exact bug MYR-454 fixes, until the next flush
// caught up.
//
// The monitor's actual job on this path is to move a car OUT of 'in_service'
// and to give a never-streamed row its first real value, so the write is
// restricted to those two states. A car the stream is actively describing is
// none of the monitor's business.
//
// Zero rows affected is a NORMAL outcome here, not ErrVehicleNotFound.
const queryUpdateVehicleStatusBaseline = `UPDATE "Vehicle"
SET "status" = $1::"VehicleStatus", "lastUpdated" = NOW()
WHERE "vin" = $2 AND "status" IN ('in_service', 'offline')`

// Drive queries. The Drive table is Prisma-owned. MYR-433 made
// `routePointsEnc` (Text?) the sole store for a drive's GPS trail: the
// plaintext `routePoints` JSONB column is never read and never written
// with real coordinates by this server again.
//
// MYR-447 did the same for the drive's four geocoded endpoint labels.
// "startLocationEnc" / "startAddressEnc" / "endLocationEnc" /
// "endAddressEnc" are now the sole store for the place names and street
// addresses of where a drive began and ended — the trail's endpoints
// rendered in the form a human reads directly.
//
// `routePoints`, `startLocation`, `startAddress`, `endLocation` and
// `endAddress` are all NOT NULL on the Prisma schema, so the INSERT still
// has to supply values for them — it writes the empty array literal `[]`
// and four empty strings, none of which carry location data. Every real
// coordinate and every real label goes to a ciphertext column only.

const queryDriveInsert = `INSERT INTO "Drive" (
	"id", "vehicleId", "date", "startTime", "endTime",
	"startLocation", "startAddress", "endLocation", "endAddress",
	"startLocationEnc", "startAddressEnc", "endLocationEnc", "endAddressEnc",
	"distanceMiles", "durationMinutes", "avgSpeedMph", "maxSpeedMph",
	"energyUsedKwh", "startChargeLevel", "endChargeLevel",
	"fsdMiles", "fsdPercentage", "interventions", "routePoints",
	"routePointsEnc"
) VALUES (
	$1, $2, $3, $4, $5,
	'', '', '', '',
	$6, $7, $8, $9,
	$10, $11, $12, $13,
	$14, $15, $16,
	$17, $18, $19, '[]'::jsonb,
	$20
)`

// queryDriveLockRoutePointsEnc reads the current ciphertext trail and
// locks the row for the read-modify-write append.
//
// The pre-MYR-433 append was a single in-database `jsonb ||` concat, which
// was atomic for free. Appending to ciphertext cannot be done in SQL — the
// server must decrypt, append, and re-seal — so the row lock is what
// preserves the same atomicity. Without it two concurrent flushes for one
// drive would each decrypt the same trail and the second write would
// silently drop the first one's points.
const queryDriveLockRoutePointsEnc = `SELECT "routePointsEnc"
FROM "Drive"
WHERE "id" = $1
FOR UPDATE`

// queryDriveSetRoutePointsEnc writes the re-sealed trail. Runs inside the
// same transaction as queryDriveLockRoutePointsEnc.
const queryDriveSetRoutePointsEnc = `UPDATE "Drive"
SET "routePointsEnc" = $2
WHERE "id" = $1`

// queryDriveComplete writes the final drive stats on drive.ended. It also
// (re)writes "startChargeLevel": the row was inserted on drive.started with
// startChargeLevel defaulting to 0 (that event carries no charge), and the
// true drive-start SOC is only known once the detector ends the drive
// (MYR-241). $14 lands that value so start/end charge are both persisted.
//
// MYR-447: $3/$4 are the SEALED end labels and land in the `*Enc`
// columns. They are nullable *string parameters — nil (an empty label,
// i.e. the geocode found nothing) writes NULL, which is the absent
// sentinel. The retired plaintext "endLocation"/"endAddress" columns are
// deliberately left untouched; `cmd/purge-plaintext-columns` scrubs them.
const queryDriveComplete = `UPDATE "Drive"
SET "endTime" = $2, "endLocationEnc" = $3, "endAddressEnc" = $4,
	"distanceMiles" = $5, "durationMinutes" = $6,
	"avgSpeedMph" = $7, "maxSpeedMph" = $8, "energyUsedKwh" = $9,
	"endChargeLevel" = $10, "fsdMiles" = $11, "fsdPercentage" = $12,
	"interventions" = $13, "startChargeLevel" = $14
WHERE "id" = $1`

// queryDriveDelete removes a Drive row outright. Used when the drive
// detector discards a micro-drive (MYR-160): the row was created on
// drive.started but the drive never met the minimum duration/distance
// thresholds, so keeping it would leak a stuck-open row (endTime
// unset). Route points live on the row itself (routePoints /
// routePointsEnc), so a single-row DELETE is complete.
const queryDriveDelete = `DELETE FROM "Drive"
WHERE "id" = $1`

const queryDriveByID = `SELECT "id", "vehicleId", "date", "startTime", "endTime",
	"startLocationEnc", "startAddressEnc", "endLocationEnc", "endAddressEnc",
	"distanceMiles", "durationMinutes", "avgSpeedMph", "maxSpeedMph",
	"energyUsedKwh", "startChargeLevel", "endChargeLevel",
	"fsdMiles", "fsdPercentage", "interventions", "createdAt",
	"routePointsEnc"
FROM "Drive"
WHERE "id" = $1`

// queryDriveListOpen returns every Drive row whose endTime is unset.
// MYR-146: read on Detector.Start to reconcile rows orphaned by a Fly
// redeploy mid-drive (in-memory vehicleState was lost; without
// reconciliation the watchdog never observes these rows and they stay
// "active" forever).
//
// Single JOIN to Vehicle is the cleanest shape because the Detector is
// global (one instance, all VINs) and keys in-memory state by VIN. A
// two-step (open drives → per-row VIN lookup) would round-trip per
// vehicle for no benefit. The set is bounded by the live-fleet size,
// and `endTime IS NULL OR endTime = ”` is by definition rare in steady
// state.
//
// Predicate covers both representations because the cleanup-binary
// findings showed the column can be either NULL or empty string
// depending on how the row was inserted. No `LIMIT` because we want
// every orphan, not a page.
const queryDriveListOpen = `SELECT
	d."id", d."vehicleId", v."vin", d."startTime", d."date", d."startChargeLevel"
FROM "Drive" d
JOIN "Vehicle" v ON d."vehicleId" = v."id"
WHERE d."endTime" IS NULL OR d."endTime" = ''`

// driveSummarySelectColumns is the lean projection used by the
// GET /api/vehicles/{vehicleId}/drives list endpoint (MYR-133, MYR-145).
// It MUST stay aligned with the wire fields emitted by
// `internal/telemetry/vehicle_drives_handler.go` driveSummary. No
// routePoints (heavy JSON blob), no energyUsedKwh / interventions
// (drive detail only).
//
// MYR-145 added the four location columns
// (`startLocation`, `startAddress`, `endLocation`, `endAddress`) to the
// projection — they're small `TEXT` columns (max a few hundred bytes
// each) so adding them keeps the per-page payload well under the
// ~5 KB-per-page budget called out in rest-api.md §5.2.2 while letting
// the SDK render origin/destination labels in the drive-history list
// without a per-row drive-detail fetch.
//
// MYR-152 added `fsdMiles` / `fsdPercentage` — two small `double`
// columns (P0, non-identifying) so the drive-history list can show FSD
// usage per drive without a per-row drive-detail fetch.
//
// MYR-447 swapped the four location columns for their `*Enc` ciphertext
// shadows. The wire shape is unchanged — scanDriveSummaryRow decrypts
// them back into the same four DriveSummaryRow string fields — but this
// list path now REQUIRES a DriveRepo built with an Encryptor. A keyless
// repo reads the labels as empty strings, exactly as it already read no
// coordinates.
//
// Cost is a wash: ciphertext is base64 and runs ~40 bytes longer than the
// address it replaces, which keeps the page well inside the ~5 KB budget.
const driveSummarySelectColumns = `"id", "vehicleId", "date", "startTime", "endTime",
	"startLocationEnc", "startAddressEnc", "endLocationEnc", "endAddressEnc",
	"distanceMiles", "durationMinutes", "avgSpeedMph", "maxSpeedMph",
	"startChargeLevel", "endChargeLevel", "fsdMiles", "fsdPercentage", "createdAt"`

// queryDriveListByVehicle is the first-page read path: returns the
// newest `limit + 1` drives for the given vehicle, ordered per
// rest-api.md §4.2.2. The +1 is the "is there more?" probe the
// handler trims back to `limit` before encoding.
const queryDriveListByVehicle = `SELECT ` + driveSummarySelectColumns + `
FROM "Drive"
WHERE "vehicleId" = $1
ORDER BY "startTime" DESC, "id" DESC
LIMIT $2`

// queryDriveListByVehicleCursor is the resume path: returns the next
// `limit + 1` drives whose (startTime, id) sorts strictly below the
// cursor anchor. The (startTime, id) tuple comparison is the
// PostgreSQL row-compare operator — it expands to the same
// `startTime < $2 OR (startTime = $2 AND id < $3)` predicate but is
// more index-friendly when a composite (startTime DESC, id DESC)
// index exists.
const queryDriveListByVehicleCursor = `SELECT ` + driveSummarySelectColumns + `
FROM "Drive"
WHERE "vehicleId" = $1
  AND ("startTime", "id") < ($2, $3)
ORDER BY "startTime" DESC, "id" DESC
LIMIT $4`

// queryDriveMissingAddresses is the MYR-240 backfill read path: every
// Drive row still missing a start address, or missing an end address on
// a row that has actually ENDED, oldest first (the backfill is a
// one-shot admin sweep, not a hot path, so ordering by createdAt just
// makes progress human-legible in logs). routePointsEnc is included
// because the Drive table carries no dedicated start/end lat/lng columns
// — the only source of coordinates to re-geocode is the first and last
// recorded route point, and since MYR-433 that trail exists only as
// ciphertext. The backfill therefore REQUIRES a usable encryption key;
// see cmd/ops/geocode.go.
//
// The end-side predicate is deliberately gated on endTime: every Drive
// row is created with endTime and endAddress both set to the empty
// string (mapDriveStarted) and only gets a real endTime when the drive
// completes (handleDriveEnded). Without the endTime guard, a backfill
// run against a still-OPEN drive would match on an empty endAddress and
// reverse-geocode the LAST recorded routePoints entry — the car's
// current, still-changing position — and persist it as the drive's
// endLocation/endAddress, which is simply wrong (and would be
// immediately stale). The "endTime IS NULL OR endTime is empty" shape
// mirrors queryDriveListOpen's predicate: the cleanup-binary findings
// showed the column can be either representation depending on how the
// row was inserted.
//
// MYR-447 REWROTE the discovery predicate, and this is the one change in
// that issue that could not be a like-for-like column swap. The old
// predicate asked `"startAddress" = <empty string>`. AES-GCM ciphertext is
// nondeterministic — a fresh 12-byte nonce per encrypt — so a sealed
// address is a different base64 string every time it is written and can
// NEVER equal the empty string again. Kept verbatim against the `*Enc`
// columns, the predicate would match nothing, forever, and the geocode
// backfill would silently report "0 rows found" on a database full of
// unaddressed drives.
//
// The nullable-`*Enc` column shape is precisely what buys the fix: NULL
// is the absent sentinel (an empty label seals to "" and is persisted as
// NULL — see label_encryption.go), so "has no address" is expressible as
// `IS NULL`, which needs to compare nothing against the ciphertext.
//
// The endTime guard is untouched: endTime is not encrypted and keeps its
// original two-representation shape.
//
// One behavioural note for the rollout: a legacy row that HAS a plaintext
// startAddress but no ciphertext sibling matches this predicate and will
// be re-geocoded. Running `backfill-location-labels` first seals those
// rows and removes them from the result set, which is both cheaper and
// preserves the original geocode.
//
// MYR-447 (audit follow-up): the projection also carries the drive's
// vehicleId and that vehicle's OWNER, joined in from "Vehicle". Neither is
// used to select rows — the discovery predicate is byte-for-byte the one
// described above — they exist because `ops geocode backfill` decrypts
// every matching row fleet-wide and must write an operator_decrypt audit
// row naming the data SUBJECT before it transmits a single coordinate to
// Mapbox. The subject of a Drive is the owner of the car that drove it,
// and there is no other column on "Drive" that names them.
//
// The join is INNER, and that is safe rather than lossy: "Drive"."vehicleId"
// is NOT NULL with a foreign key to "Vehicle"."id", so every Drive row has
// exactly one matching Vehicle and the join cannot drop a row the old
// single-table SELECT would have returned. A row it DID drop would be a
// row with no identifiable owner — which the backfill must not decrypt
// anyway, because it could not be audited.
const queryDriveMissingAddresses = `SELECT d."id", d."startAddressEnc", d."endAddressEnc", d."endTime", d."routePointsEnc",
       d."vehicleId", v."userId"
FROM "Drive" d
JOIN "Vehicle" v ON v."id" = d."vehicleId"
WHERE d."startAddressEnc" IS NULL OR (d."endAddressEnc" IS NULL AND NOT (d."endTime" IS NULL OR d."endTime" = ''))
ORDER BY d."createdAt" ASC`

// queryDriveUpdateAddresses writes whichever of the four location
// columns the caller supplies; COALESCE leaves the rest untouched. Every
// argument is a nilable *string so the backfill can pass nil for the
// side it didn't (re)geocode this run — e.g. only startAddress was empty,
// or the end-side geocode lookup returned ErrNoResult — without
// clobbering a value the row already had or that a concurrent write
// landed between the backfill's SELECT and this UPDATE.
//
// MYR-447: the parameters are SEALED labels and the targets are the
// `*Enc` columns. The COALESCE shape is unchanged and still means
// "leave this column exactly as it was" — which is the only reason the
// nondeterminism of the ciphertext is harmless here: nothing compares
// the new value to the old one, it either replaces it or is NULL.
const queryDriveUpdateAddresses = `UPDATE "Drive"
SET "startLocationEnc" = COALESCE($2, "startLocationEnc"),
    "startAddressEnc"  = COALESCE($3, "startAddressEnc"),
    "endLocationEnc"   = COALESCE($4, "endLocationEnc"),
    "endAddressEnc"    = COALESCE($5, "endAddressEnc")
WHERE "id" = $1`

// Account queries. The Account table is Prisma-owned (NextAuth). We read
// tokens and update them in-place when refreshing expired OAuth tokens.
//
// MYR-433 ended the MYR-62 dual-write regime on this server: Tesla OAuth
// tokens are read from and written to the `*_enc` (AES-256-GCM
// ciphertext) columns ONLY. A Tesla access/refresh token in plaintext is
// the sharpest item in the whole database — a leak of these columns is
// fleet control over a user's car — so this server does not select them
// and does not set them.
//
// The plaintext columns still EXIST on the Prisma-owned table (dropping
// them is a react-frontend Prisma migration; CG-DL-9 forbids Go
// migrations touching Prisma tables). `cmd/purge-plaintext-columns`
// NULLs their residual values once the ciphertext sibling verifies.
// See docs/contracts/data-classification.md §3.3.

// #nosec G101 -- column-name SQL, not a credential. gosec greps the
// constant for "access_token" / "refresh_token" / "id_token" and flags
// the literal as a hardcoded credential by mistake.
const queryTeslaToken = `SELECT
    "access_token_enc",
    "refresh_token_enc",
    "id_token_enc",
    "expires_at"
FROM "Account"
WHERE "userId" = $1 AND "provider" = 'tesla'
LIMIT 1`

// queryUpdateTeslaToken writes a refreshed token pair as ciphertext, and
// NULLs the legacy plaintext columns while it is there.
//
// The NULLs make this self-healing. A token refresh is the moment a
// pre-MYR-433 plaintext credential becomes not merely exposed but WRONG —
// it is a superseded token sitting in a readable column — so scrubbing it
// here means an account heals itself on its next refresh (Tesla tokens are
// short-lived, so that is hours) without waiting for
// cmd/purge-plaintext-columns to be run. queryProvisionAccount does the
// same on re-link; between them, every Go-side Account write leaves the
// row clean.
const queryUpdateTeslaToken = `UPDATE "Account"
SET "access_token_enc" = $1,
    "refresh_token_enc" = $2,
    "expires_at" = $3,
    "access_token" = NULL,
    "refresh_token" = NULL
WHERE "userId" = $4 AND "provider" = 'tesla'`
