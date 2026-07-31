package mask

import (
	"github.com/myrobotaxi/telemetry/internal/auth"
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
	// VehicleState shape). Viewers see the same set EXCEPT the full
	// `vin`, which MYR-279 gated to the owner role.
	//
	// MYR-286: `licensePlate` is in BOTH role allow-lists. That is a
	// DELIBERATE PRODUCT DECISION, not an oversight, and it reverses
	// the forward-looking owner-only placeholder this table carried
	// before the field was ever on the wire: the entire purpose of the
	// owner-entered plate is that a RIDER can identify the correct car
	// at pickup, which fails if only the owner can see it. Contrast
	// `vin` (MYR-279), which stays owner-only — a VIN identifies the
	// physical car and links to its location history, while a plate is
	// the label the rider is standing on the curb reading. Both are
	// P1; the asymmetry is about who needs the value, not about tier.
	ResourceVehicleState: {
		auth.RoleOwner:  setFromFields(vehicleStateOwnerFields),
		auth.RoleViewer: setFromFields(vehicleStateViewerFields),
	},

	// rest-api.md §5.2.0 — Vehicles list (vehicle summary). Owners and
	// viewers see the same field set; the viewer list adds
	// `sharePermission` and subtracts NOTHING. `licensePlate` (MYR-286)
	// is in both lists for the same deliberate reason as the snapshot
	// resource above — a rider identifies the car at pickup from the
	// catalog row.
	//
	// MYR-184 REMOVED the previous `name` subtraction. Two reasons, and
	// the second is the binding one:
	//
	//   - Product: the rider UI renders "{Owner}'s {Vehicle}", so the
	//     nickname is the label a viewer reads. Vehicle nicknames are
	//     P1-acceptable for viewers under the same policy that puts
	//     first names in the push payloads.
	//   - Contract: `name` is in the `required` list of
	//     schemas/vehicle-summary.schema.json. Stripping it made EVERY
	//     viewer row — the §7.0 merge and the §7.5.5 redeem response
	//     alike — fail its own schema. A field this mask removes must
	//     be OPTIONAL in the schema; that is asserted directly by
	//     TestViewerMaskKeepsEverySchemaRequiredField in
	//     internal/telemetry.
	//
	// The viewer merge is live (MYR-184), so this projection runs on
	// every list call from anyone who has redeemed an invite.
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
// "properties" (which since MYR-286 includes the Prisma-owned
// licensePlate column). See rest-api.md §5.2.1.
var vehicleStateOwnerFields = []string{
	// Identity (DB-sourced, not telemetry).
	"vehicleId",
	// vin is the FULL 17-char VIN (MYR-279). Owner-only: it is deliberately
	// absent from the viewer allow-list (vehicleStateViewerFields removes it)
	// and never appears on the WS broadcast — the vehicles-list surfaces
	// vinLast4 instead. See data-classification.md section 1.3.
	"vin",
	"name",
	"model",
	"year",
	"color",
	// MYR-279 vehicle-detail read-backs (P0, non-identifying, side-table sourced).
	"softwareVersion",
	"trim",
	// MYR-320 vehicle-detail read-backs. P0 BOTH ROLES, matching their direct
	// siblings exactly rather than being reasoned about afresh: `trimLabel` is
	// an equipment/trim fact, the same tier as `trim`/`model`/`year`, and
	// `fsdVersion` is a software designation, the same tier as
	// `softwareVersion` — all four of which are already viewer-visible (the
	// viewer list below removes ONLY `vin`). Nothing here identifies a person
	// or links to a physical car, which is the property that makes `vin` the
	// one owner-only snapshot field. Snapshot-only (REST-derived; Tesla streams
	// neither), and deliberately NOT on the vehicles-list row.
	"trimLabel",
	"fsdVersion",
	// licensePlate (MYR-286) — owner-entered, P1, and in the VIEWER
	// allow-list too (vehicleStateViewerFields removes only `vin`).
	// Deliberate product decision: the plate exists so a rider can
	// identify the car at pickup. Do NOT "fix" this to owner-only by
	// analogy with `vin` above — see the ResourceVehicleState comment.
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
	// Cabin controls read-back (MYR-252). Individually-delivered
	// vehicle_update fields — owners see the live state of climate, lock,
	// seats, charge-port, trunk/frunk, and media so the app can render
	// honest control state instead of only command-ack optimism (MYR-251).
	// Classified P0 in vehicle-state.schema.json (not identifying — same
	// tier as chargeLevel/speed/gearPosition). rest-api.md §5.2.1 mirrors
	// this list. Not currently emitted on the DB-backed /snapshot (WS-live
	// only in this iteration — see rest-api.md §7.1).
	"locked",
	"hvacPower",
	"isClimateOn",
	"fanSpeed",
	"driverTempSetting",
	"passengerTempSetting",
	"hvacAutoMode",
	"hvacAcEnabled",
	"seatHeaterLeft",
	"seatHeaterRight",
	"seatHeaterRearLeft",
	"seatHeaterRearCenter",
	"seatHeaterRearRight",
	"seatCoolerLeft",
	"seatCoolerRight",
	"seatVentEnabled",
	"chargePortDoorOpen",
	"frunkOpen",
	"trunkOpen",
	"mediaPlaybackStatus",
	"mediaVolume",
	// MYR-303 media NOW-PLAYING block.
	//
	// The five free-text fields (title/artist/album/station/playbackSource) are
	// P1, not P0 like the rest of this cabin block — they are free-text USER
	// CONTENT, and an accumulated stream of them reveals listening habits
	// (taste, and by inference language, mood, politics, religion).
	//
	// They are nonetheless in BOTH role allow-lists, and that is the whole
	// point of the feature rather than an oversight: a RIDER sitting in the car
	// can hear what is playing, so showing it to them reveals nothing they do
	// not already know, and a now-playing panel that goes blank for the
	// passenger is the feature failing. Same reasoning as `licensePlate`
	// (MYR-286) — the asymmetry that matters is who NEEDS the value, not the
	// tier. Contrast `vin`, which stays the one owner-only snapshot field.
	//
	// What P1 DOES buy here is handled outside this table: never log the values
	// (presence/length only — see data-classification.md §1.13), never emit
	// them outside the vehicle's party, never aggregate or retain them as a
	// listening history.
	//
	// NOTE for the FR-5.5 `limited_viewer` seam (rest-api.md §5.2.1): these five
	// MUST be EXCLUDED from that tier when it is implemented, alongside precise
	// GPS and the navigation group. limited_viewer is the deliberately-degraded
	// tier for someone who is NOT in the car — the "they can already hear it"
	// justification above evaporates, and free-text listening data is exactly
	// what that tier exists to withhold. The three numerics below are P0 and may
	// stay. v1 has no limited_viewer role, so there is nothing to encode here
	// yet; this comment is the standing instruction for whoever adds it.
	"mediaNowPlayingTitle",
	"mediaNowPlayingArtist",
	"mediaNowPlayingAlbum",
	"mediaNowPlayingStation",
	"mediaPlaybackSource",
	// P0 numerics — a bare track length, playback offset, or volume ceiling
	// identifies nothing on its own (same tier as the sibling mediaVolume).
	"mediaNowPlayingDurationMs",
	"mediaNowPlayingElapsedMs",
	"mediaVolumeMax",
	// MYR-308 — ventilated-seat capability. P0 both roles: an equipment/trim
	// fact about the car, the same tier as `trim`/`model`/`year` and reasoned
	// about the same way. Snapshot-only (Tesla does not stream it).
	"seatCoolingCapable",
	// MYR-316 — service window. P0 both roles: operational timing about the
	// car, the same tier as the sibling `status` it qualifies, and a rider
	// needs it for exactly the same reason the owner does (it floors the
	// scheduling picker). Snapshot-only; server-computed, never client-set.
	"serviceEstimatedEndAt",
	// MYR-342 — the owner's ride-sharing switch. P0 both roles: operational
	// availability of the car, the same tier as `status` and `hasActiveRide`.
	// EXPLICITLY not owner-private, and the reasoning is the inverse of the
	// usual one — a viewer is the party the value is ABOUT. A rider who cannot
	// see that the car shared with them is paused discovers it from a 409 after
	// filling in a pickup and a dropoff, which is the feature failing. The
	// viewer list below inherits it (it subtracts only `vin`).
	"rideShareEnabled",
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

// vehicleStateViewerFields is owner minus the full vin (MYR-279) — and
// NOTHING else. The full VIN is owner-only (party-scoped): a viewer with shared
// access sees model/year/color/softwareVersion/trim but NOT the full VIN, which
// links to the physical car / its location history (data-classification.md
// §1.3, §2.1). softwareVersion and trim stay visible to viewers (P0,
// non-identifying, same tier as model), and since MYR-320 so do their siblings
// trimLabel and fsdVersion, for exactly the same reason.
//
// MYR-286 REMOVED the previous `licensePlate` exclusion. The plate is now a
// both-roles field by deliberate product decision (a rider must be able to read
// the plate of the car pulling up); `vin` is the one field that stays
// owner-only. Built by exclusion to avoid drift.
var vehicleStateViewerFields = removeField(vehicleStateOwnerFields, "vin")

// vehicleSummaryOwnerFields is the v1 owner allow-list for the
// vehicles-list catalog response (rest-api.md §5.2.0 / §7.0). Thin
// catalog only — no GPS, no nav, no climate. SDK consumers calling
// `client.vehicles.list()` get these fields per row; per-vehicle
// telemetry is fetched via `/snapshot` (§7.1).
var vehicleSummaryOwnerFields = []string{
	"vehicleId",
	// name is the owner-curated nickname (P1). Viewer-visible since
	// MYR-184: the rider UI renders "{Owner}'s {Vehicle}", and it is
	// `required` in vehicle-summary.schema.json, so it cannot be masked
	// away without making every viewer row schema-invalid.
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
	// MYR-233 — derived operational state (is the car serving a ride
	// right now?), P0 like its sibling `status`. Riders need it to
	// render a Busy badge and route new instant requests to the
	// scheduling flow, so it is NOT owner-private: the viewer list
	// below inherits it (the viewer list subtracts nothing).
	"hasActiveRide",
	// MYR-286 — owner-entered license plate (P1). Like `hasActiveRide`,
	// it is NOT owner-private: the viewer list below inherits it because
	// a rider identifies the car at pickup from this catalog row —
	// exactly the reason `name` is viewer-visible too, since MYR-184.
	// Deliberate product decision;
	// contrast the full `vin`, which the catalog never carries at all
	// (`vinLast4` only) and which the snapshot gates to owners.
	"licensePlate",
	// MYR-316 — service window. P0, and NOT owner-private for the same reason
	// as `hasActiveRide`: the rider scheduling flow floors its picker at this
	// instant, so a viewer who cannot see it cannot schedule correctly. The
	// viewer list below inherits it (the viewer list subtracts nothing).
	"serviceEstimatedEndAt",
	// MYR-342 — the owner's ride-sharing switch. P0, and NOT owner-private for
	// a stronger reason than any of the fields above: the viewer is the party
	// this value is ABOUT. It tells a rider whether the car they were granted
	// is taking requests at all, and withholding it would mean the only way to
	// learn a car is paused is to be refused with 409 `vehicle_unavailable`
	// after composing a whole request. The viewer list below inherits it (that
	// list subtracts nothing).
	"rideShareEnabled",
}

// vehicleSummaryViewerFields is the owner list PLUS `sharePermission`, with
// NOTHING subtracted (rest-api.md §5.2.0).
//
// `name` used to be subtracted here. It is not any more: the rider UI renders
// the shared car as "{Owner}'s {Vehicle}", so the owner-curated nickname is the
// label the viewer reads, and a vehicle nickname is P1-acceptable for viewers
// under the same policy that puts the owner's first name in the push payloads
// and in RedeemShareInviteResponse.ownerFirstName. It was also a CONTRACT
// violation: `name` is `required` in schemas/vehicle-summary.schema.json, so
// stripping it made every viewer row invalid against the shape its own consumer
// decodes. Anything removed here in future MUST be optional in that schema.
//
// MYR-184 added `sharePermission`, and it is the first field that is
// VIEWER-ONLY rather than owner-only — the asymmetry runs the other way for
// once. It describes the access the caller holds over a car they do NOT own, so
// it is meaningless on an owner row (an owner holds no grant) and is
// deliberately absent from the owner list above rather than being emitted
// empty. P0: an authorization value describing a relationship, not identifying
// data — the same classification as its sibling `role`.
//
// MYR-369 made it DERIVED from the grant's flags rather than a stored tier, and
// a SUSPENDED grant produces no viewer row at all — so this allow-list never
// projects one, and there is no "suspended" field here to add.
//
// The owner list is COPIED before the append: appending straight onto it would
// share backing array with vehicleSummaryOwnerFields and could overwrite an
// owner entry the next time the owner list grows.
var vehicleSummaryViewerFields = append(
	append([]string(nil), vehicleSummaryOwnerFields...),
	"sharePermission",
)

// driveSummaryFields is the per-row drive-list allow-list shared by
// owner and viewer per rest-api.md §5.2.2.
//
// MYR-145 added the four start/end Location + Address fields. They
// carry the reverse-geocoded labels written by
// `internal/store/writer_drives.go` and let the SDK render origin /
// destination strings in the drive-history list without a per-row
// drive-detail fetch. Same allow-list for owner and viewer — viewers
// already see them on the drive-detail endpoint per §5.2.3, so
// surfacing them on the list keeps the two payloads consistent and is
// the whole point of the FR-5.1 sharing use case.
var driveSummaryFields = []string{
	"id",
	"vehicleId",
	"startTime",
	"endTime",
	"date",
	"startLocation",
	"startAddress",
	"endLocation",
	"endAddress",
	"distanceMiles",
	"durationSeconds",
	"avgSpeedMph",
	"maxSpeedMph",
	"startChargeLevel",
	"endChargeLevel",
	"fsdMiles",
	"fsdPercentage",
	"createdAt",
}

// driveDetailFields is the drive-detail allow-list shared by owner and
// viewer per rest-api.md §5.2.3. Includes start/end location and
// address (rationale: FR-5.1 sharing use case).
//
// MYR-130: `date` was added to close a mask/OpenAPI drift — the
// `DriveDetail` OpenAPI component (specs/rest.openapi.yaml) marks `date`
// as required and the §7.3 / fixture response bodies include it, but the
// allow-list had omitted it, which would have stripped `date` from the
// masked response and broken OpenAPI conformance for the new
// GET /api/drives/{driveId} handler. §5.2.3 updated in lockstep.
var driveDetailFields = []string{
	"id",
	"vehicleId",
	"startTime",
	"endTime",
	"date",
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

// inviteOwnerFields is the owner-visible ShareInvite shape per rest-api.md
// §7.5 and schemas/vehicle-sharing.schema.json (contracts v0.19.0).
//
// MYR-184 REBUILT THIS LIST. It previously described a shape that never
// existed on this server: an `id`/`email` invite modelled on the retired
// Prisma `Invite` table, plus a `revokedAt` field. That drift was harmless
// only because nothing projected through it; now that the endpoints are real,
// the list is the allow-list an owner's invite rows are actually filtered
// through, so every name here is the wire name the handler emits.
//
// Three corrections worth naming, because each was a real field on the old
// list or a real omission from it:
//
//   - `email` is GONE and has no replacement. This contract is CODE-based —
//     there is no email infrastructure and no address is ever collected. Its
//     stand-in is `label`, an owner-typed memo ("Mom", "Mira Chen") that is
//     never resolved to an account.
//   - `revokedAt` is GONE. Revocation is a server-side tombstone; a revoked
//     row is never serialized at all, so a field describing when it happened
//     has no wire moment to appear in. Keeping it would imply revoked rows
//     are returned.
//   - `code` is NEW, and is the one field here that is a live CREDENTIAL
//     rather than a description of one. It is owner-only by construction —
//     the viewer role has no entry in this resource at all — and the handler
//     additionally omits it from any row that is not pending. Never log it.
//
// `id` became `inviteId` to match the wire; `expiresAt` was missing entirely.
var inviteOwnerFields = []string{
	"inviteId",
	"vehicleId",
	// label is P1 — a person's name, typed by the owner. Included for
	// owners because it is THEIR memo about a person they chose to invite;
	// it is never delivered to the invited party.
	"label",
	"permission",
	"status",
	// allowRides / suspended are the MYR-369 per-grant flags — accepted rows
	// only, and P0 (an authorization capability and an authorization state,
	// the same tier as `permission` and `status` beside them). Owner-only by
	// construction like every field here: the viewer role has no entry in
	// this resource at all, and a viewer must not be able to read the
	// controls their own access is governed by.
	"allowRides",
	"suspended",
	// code is P1 and BEARER. Present only on pending rows (enforced in the
	// handler and again in SQL); never logged, never echoed into an error.
	"code",
	// shareUrl (MYR-368) is the signed join link. It CONTAINS the code, so
	// it is the same tier and the same handling rule as the line above —
	// P1, bearer, pending rows only, never logged. Listed separately rather
	// than folded into `code` because the mask is a flat allow-list of wire
	// names: an entry missing here is a field silently dropped from the
	// response, which for this one would look like "signed links stopped
	// working" with nothing to point at.
	"shareUrl",
	"createdAt",
	"expiresAt",
	"acceptedAt",
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
// exclusion (e.g., owner minus `vin`). The loop never breaks on
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
