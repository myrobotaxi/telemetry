package mask

import (
	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
)

// masksByResource is the v1 per-(resource, role) field-mask matrix.
// Source-of-truth: docs/contracts/rest-api.md §5.2. Every change here
// MUST be made in lockstep with that matrix or contract-guard CG-DC-5
// will block the PR. See docs/contracts/data-classification.md §5.
//
// FR-5.5 third-role extension seam: adding a new role is a one-file
// change in this table — append a new auth.Role entry under each
// resource's role-table sibling map.
var masksByResource = map[ResourceType]map[auth.Role]ResourceMask{
	// rest-api.md §5.2.1 — Vehicle snapshot. Owners see every field
	// in docs/contracts/schemas/vehicle-state.schema.json (the v1
	// VehicleState shape). Viewers see the same set EXCEPT
	// licensePlate. NOTE: licensePlate is a Prisma-owned column per
	// data-classification.md §1.3 and is NOT currently a member of
	// vehicle-state.schema.json — the rule below is forward-looking
	// (codifies the behavior the first time licensePlate is surfaced
	// over the SDK). Including licensePlate in the owner allow-list
	// today is harmless: input payloads that lack the field simply do
	// not produce a key in the projected output.
	ResourceVehicleState: {
		auth.RoleOwner:  setFromFields(vehicleStateOwnerFields),
		auth.RoleViewer: setFromFields(vehicleStateViewerFields),
	},

	// rest-api.md §5.2.0 — Vehicles list (vehicle summary). Owners see
	// every field; viewers see every field EXCEPT `name` (P1,
	// owner-curated nickname). Per §7.0, v1 only reaches the owner
	// branch (viewer-merged enumeration depends on the Go server
	// reading the Prisma-owned Invite table — PLANNED), but the viewer
	// mask is wired now so the data-side is ready when the invite-read
	// pathway lands.
	ResourceVehicleSummary: {
		auth.RoleOwner:  setFromFields(vehicleSummaryOwnerFields),
		auth.RoleViewer: setFromFields(vehicleSummaryViewerFields),
	},

	// rest-api.md §5.2.2 — Drive list (drive summary). Owners and
	// viewers see the same field set; viewers are read-only per
	// FR-5.4 but observe the same data. startAddress / startLocation
	// / endAddress / endLocation are deliberately NOT in this
	// resource — they are returned by the drive detail endpoint
	// (§5.2.3) to keep list payloads lean.
	ResourceDriveSummary: {
		auth.RoleOwner:  setFromFields(driveSummaryFields),
		auth.RoleViewer: setFromFields(driveSummaryFields),
	},

	// rest-api.md §5.2.3 — Drive detail. Owner and viewer share the
	// same field set including start/end location and address. The
	// rationale is FR-5.1: the entire point of sharing is for the
	// viewer to know where the drive started and ended. routePoints
	// is intentionally NOT here — it has its own resource (§5.2.4 /
	// ResourceDriveRoute) for lazy-fetch reasons (heavy payload).
	ResourceDriveDetail: {
		auth.RoleOwner:  setFromFields(driveDetailFields),
		auth.RoleViewer: setFromFields(driveDetailFields),
	},

	// rest-api.md §5.2.4 — Drive route. Both roles see the full
	// polyline; a partial route would defeat FR-5.1. Only one field:
	// routePoints.
	ResourceDriveRoute: {
		auth.RoleOwner:  setFromFields(driveRouteFields),
		auth.RoleViewer: setFromFields(driveRouteFields),
	},

	// rest-api.md §5.2.5 — Invite endpoints. Owner-only at the
	// routing layer (viewers receive 403 before reaching the mask).
	// The viewer entry is intentionally absent so CG-DC-5 sees that
	// viewers have no allow-list for invites — fail-closed produces
	// deny-all. The owner allow-list mirrors the response shape in
	// rest-api.md §7.5.
	ResourceInvite: {
		auth.RoleOwner: setFromFields(inviteOwnerFields),
		// auth.RoleViewer intentionally omitted — fail-closed deny-all.
	},
}

// vehicleStateOwnerFields is the v1 owner allow-list for the vehicle
// snapshot. Sourced from docs/contracts/schemas/vehicle-state.schema.json
// "properties" plus the Prisma-owned licensePlate column (forward-
// looking, see rest-api.md §5.2.1).
var vehicleStateOwnerFields = []string{
	// Identity (DB-sourced, not telemetry).
	"vehicleId",
	"vin",
	"name",
	"model",
	"year",
	"color",
	"licensePlate",
	// Charge atomic group.
	"chargeLevel",
	"chargeState",
	"estimatedRange",
	"timeToFull",
	// Gear atomic group.
	"status",
	"gearPosition",
	// Speed / GPS atomic group.
	"speed",
	"heading",
	"latitude",
	"longitude",
	"locationName",
	"locationAddress",
	// Climate / cabin.
	"interiorTemp",
	"exteriorTemp",
	// Odometer / FSD.
	"odometerMiles",
	"fsdMilesSinceReset",
	// Misc identity / pairing flags.
	"virtualKeyPaired",
	// Navigation atomic group. Wire field names per
	// vehicle-state.schema.json (destinationName, destinationAddress,
	// destinationLatitude, destinationLongitude, originLatitude,
	// originLongitude, etaMinutes, tripDistanceRemaining,
	// navRouteCoordinates).
	"destinationName",
	"destinationAddress",
	"destinationLatitude",
	"destinationLongitude",
	"originLatitude",
	"originLongitude",
	"etaMinutes",
	"tripDistanceRemaining",
	"navRouteCoordinates",
	// Aliases used in some snapshot payloads (rest-api.md §7.1
	// references navDestinationName etc.). Including both the schema
	// names above and the snapshot-aliased forms keeps the mask
	// resilient to whichever shape the handler emits today.
	"navDestinationName",
	"navDestinationLocation",
	"navOriginLocation",
	"navEtaMinutes",
	"navTripDistanceRemaining",
	// driveTrailCoordinates is the per-drive accumulated GPS trail
	// emitted by internal/ws/route_broadcast.go ("where the car has
	// been"). Distinct from the navigation atomic group's
	// navRouteCoordinates, which carries Tesla's planned route
	// polyline ("where the car is going"). See
	// docs/contracts/websocket-protocol.md §4.1.6.
	"driveTrailCoordinates",
	// Wire freshness marker.
	"lastUpdated",
}

// vehicleStateViewerFields is owner minus licensePlate, per
// rest-api.md §5.2.1. Built lazily in init() to avoid drift.
var vehicleStateViewerFields = removeField(vehicleStateOwnerFields, "licensePlate")

// vehicleSummaryOwnerFields is the v1 owner allow-list for the
// vehicles-list catalog response (rest-api.md §5.2.0 / §7.0). Thin
// catalog only — no GPS, no nav, no climate. SDK consumers calling
// `client.vehicles.list()` get these fields per row; per-vehicle
// telemetry is fetched via `/snapshot` (§7.1).
var vehicleSummaryOwnerFields = []string{
	"vehicleId",
	"name",
	"model",
	"year",
	"color",
	"vinLast4",
	"status",
	"chargeLevel",
	"estimatedRange",
	"lastUpdated",
	"role",
}

// vehicleSummaryViewerFields is owner minus `name` per rest-api.md
// §5.2.0. The user-assigned nickname is P1 and stays owner-only;
// viewers still see model/year/color so they can identify the car in
// their list.
var vehicleSummaryViewerFields = removeField(vehicleSummaryOwnerFields, "name")

// driveSummaryFields is the per-row drive-list allow-list shared by
// owner and viewer per rest-api.md §5.2.2. Excludes startAddress /
// startLocation / endAddress / endLocation — those are drive-detail
// fields.
var driveSummaryFields = []string{
	"id",
	"vehicleId",
	"startTime",
	"endTime",
	"date",
	"distanceMiles",
	"durationSeconds",
	"avgSpeedMph",
	"maxSpeedMph",
	"startChargeLevel",
	"endChargeLevel",
	"createdAt",
}

// driveDetailFields is the drive-detail allow-list shared by owner and
// viewer per rest-api.md §5.2.3. Includes start/end location and
// address (rationale: FR-5.1 sharing use case).
var driveDetailFields = []string{
	"id",
	"vehicleId",
	"startTime",
	"endTime",
	"distanceMiles",
	"durationSeconds",
	"avgSpeedMph",
	"maxSpeedMph",
	"energyUsedKwh",
	"startChargeLevel",
	"endChargeLevel",
	"fsdMiles",
	"fsdPercentage",
	"interventions",
	"startLocation",
	"startAddress",
	"endLocation",
	"endAddress",
	"createdAt",
}

// driveRouteFields is the heavy-payload route response per
// rest-api.md §5.2.4. Single field.
var driveRouteFields = []string{
	"routePoints",
}

// inviteOwnerFields is the owner-visible Invite shape per
// rest-api.md §7.5.2. email is P1 per data-classification.md §1.6 but
// the owner already knows who they invited; the field is intentionally
// included for owners.
var inviteOwnerFields = []string{
	"id",
	"vehicleId",
	"email",
	"status",
	"createdAt",
	"acceptedAt",
	"revokedAt",
}

// setFromFields converts a slice of field names into a set keyed by
// name. A small helper to keep the matrix declarations terse without
// resorting to a builder.
func setFromFields(fields []string) ResourceMask {
	allowed := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		allowed[f] = struct{}{}
	}
	return ResourceMask{Allowed: allowed}
}

// removeField returns a copy of fields with all occurrences of name
// removed. Used to derive viewer allow-lists from owner allow-lists by
// exclusion (e.g., owner minus licensePlate). The loop never breaks on
// match — every input element that equals name is filtered out.
func removeField(fields []string, name string) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == name {
			continue
		}
		out = append(out, f)
	}
	return out
}
