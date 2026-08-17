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
	// VehicleState shape).
	//
	// MYR-435 (client decision, 2026-08-02): the viewer arm is NO LONGER
	// "owner minus vin". It is an explicit allow-list — location, route,
	// identity, charge, availability, freshness — with media, cabin
	// climate, and all vehicle-controls state withheld. All THREE delivery
	// surfaces read THIS table, so the narrowing applies to each by
	// construction. That is pinned per-surface, by tests that drive the
	// actual delivery path and iterate OwnerOnlyVehicleStateFields():
	//
	//   REST snapshot ......... internal/telemetry,
	//     TestVehicleSnapshotHandler_ViewerSnapshotOmitsEveryOwnerOnlyField
	//   WS live broadcast ..... internal/ws,
	//     TestBroadcaster_CabinOnlyTelemetry_SendsViewerNothing
	//   WS connect-time replay  internal/ws,
	//     TestHub_SendSnapshot_ViewerReplayOmitsEveryOwnerOnlyField
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
	// `softwareVersion` — all four of which are viewer-visible. Nothing here
	// identifies a person or links to a physical car. MYR-435 did not touch
	// them: they are identity/equipment, not media, cabin, or controls.
	// Snapshot-only (REST-derived; Tesla streams neither), and deliberately NOT
	// on the vehicles-list row.
	"trimLabel",
	"fsdVersion",
	// licensePlate (MYR-286) — owner-entered, P1, and in the VIEWER
	// allow-list too. Deliberate product decision: the plate exists so a
	// rider can identify the car at pickup. Do NOT "fix" this to owner-only
	// by analogy with `vin` above — see the ResourceVehicleState comment.
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
	// Climate / cabin. OWNER-ONLY since MYR-435 — see
	// vehicleStateOwnerOnlyFields for the client decision and the
	// `exteriorTemp` ambiguity call.
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
	//
	// MYR-435: this entire block is OWNER-ONLY. P0 was never the question —
	// these fields exist to render the owner's control tiles, and a viewer
	// has no control tiles to render.
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
	// MYR-303 media NOW-PLAYING block. OWNER-ONLY since MYR-435.
	//
	// The five free-text fields (title/artist/album/station/playbackSource) are
	// P1, not P0 like the rest of this cabin block — they are free-text USER
	// CONTENT, and an accumulated stream of them reveals listening habits
	// (taste, and by inference language, mood, politics, religion).
	//
	// They used to be in BOTH role allow-lists on the reasoning that a rider in
	// the car can already hear what is playing. MYR-435 retired that argument —
	// a share grant is durable and remote, so a "viewer" is frequently not in
	// the car at all. The full reasoning lives in vehicleStateOwnerOnlyFields.
	//
	// What P1 buys here is still handled outside this table: never log the
	// values (presence/length only — see data-classification.md §1.13), never
	// emit them outside the vehicle's party, never aggregate or retain them as
	// a listening history.
	//
	// The FR-5.5 `limited_viewer` seam no longer needs a standing note about
	// these five: MYR-435 excluded them for EVERY non-owner role, so a future
	// limited_viewer inherits the exclusion by construction.
	"mediaNowPlayingTitle",
	"mediaNowPlayingArtist",
	"mediaNowPlayingAlbum",
	"mediaNowPlayingStation",
	"mediaPlaybackSource",
	// P0 numerics — a bare track length, playback offset, or volume ceiling
	// identifies nothing on its own (same tier as the sibling mediaVolume).
	// Owner-only anyway since MYR-435: elapsed + duration is a live "someone
	// is listening right now" occupancy signal even without the track name.
	"mediaNowPlayingDurationMs",
	"mediaNowPlayingElapsedMs",
	"mediaVolumeMax",
	// MYR-308 — ventilated-seat capability. An equipment/trim fact, but
	// OWNER-ONLY since MYR-435: unlike trim/model it has exactly one consumer,
	// the owner's seat-cooling control tile, which viewers no longer render.
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
	// filling in a pickup and a dropoff, which is the feature failing.
	// MYR-435 kept it viewer-visible for exactly that reason.
	"rideShareEnabled",
	// MYR-491 — the setup state of a car that is not yet streaming. P0 both
	// roles: operational state about the CAR, the same tier as `status`, which
	// it exists to qualify ("offline" vs "offline BECAUSE the virtual key is
	// not paired yet"). It carries no VIN, no coordinate, no token and no user
	// content — an enum member plus a timestamp — so it is log-safe in full.
	//
	// Viewer-visible for the same reason as `rideShareEnabled`, and the
	// argument is if anything stronger: MYR-437's rider-side picker renders a
	// never-streamed shared car as permanently "offline", and the honest
	// answer — "this car is still being set up" — is precisely what the viewer
	// needs before trying to book it. Withholding it would leave the picker
	// with the wrong word for the only state it gets wrong today.
	"setupState",
	// Odometer / FSD. Both roles — neither media, cabin, nor a control.
	"odometerMiles",
	"fsdMilesSinceReset",
	// Misc identity / pairing flags. `virtualKeyPaired` is OWNER-ONLY since
	// MYR-435 — it gates whether COMMANDS can be sent, and viewers send none.
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

// vehicleStateViewerFields is the v1 VIEWER allow-list for the vehicle
// snapshot and every masked WS vehicle_update frame.
//
// MYR-435 (CLIENT DECISION, 2026-08-02) REBUILT THIS LIST AND INVERTED HOW IT
// IS CONSTRUCTED. It used to be `removeField(vehicleStateOwnerFields, "vin")` —
// owner-minus-VIN, a SUBTRACTION. The MYR-427 privacy audit found that shape is
// the defect, not the contents: under subtraction, every field added to the
// owner list reaches viewers by DEFAULT, silently, with the decision made by
// whoever forgot to think about it. That is how a viewer came to receive the
// owner's now-playing track and cabin temperature.
//
// This is now an EXPLICIT ALLOW-LIST written out in full. The cost is that a
// new owner field does not reach viewers until someone adds it here; that cost
// is the point (fail-closed). TestVehicleStateRoleListsPartitionOwnerFields
// makes the omission LOUD rather than silent: every owner field must appear in
// either this list or vehicleStateOwnerOnlyFields, so a new field cannot be
// added without classifying it.
//
// The client's instruction was: "Remove media/cabin and any vehicle controls."
// What a viewer KEEPS is what the viewing/riding features actually consume —
// where the car is, where it is going, which car it is, and whether it can make
// the trip. See vehicleStateOwnerOnlyFields below for the removals and the
// per-group reasoning.
var vehicleStateViewerFields = []string{
	// Identity. `vin` is absent (MYR-279, still owner-only); the rest of the
	// identity block is how a rider recognizes the car at the curb.
	// `licensePlate` is here by deliberate product decision (MYR-286) — it is
	// literally the label the rider is standing on the sidewalk reading.
	"vehicleId",
	"name",
	"model",
	"year",
	"color",
	"softwareVersion",
	"trim",
	"trimLabel",
	"fsdVersion",
	"licensePlate",
	// Charge atomic group. A rider deciding whether this car can make the trip
	// needs the level, the range, and whether it is plugged in and climbing.
	// NOTE the boundary drawn against `chargePortDoorOpen`, which is removed:
	// "is it charging" is trip-planning state, "is the flap open" is an owner
	// control tile.
	"chargeLevel",
	"chargeState",
	"estimatedRange",
	"timeToFull",
	// Gear atomic group. `status` is the availability/in-service value the
	// rider UI badges from; `gearPosition` is its atomic-group sibling and is
	// motion state (P/R/N/D), not a control the viewer could actuate. Splitting
	// an atomic group is its own contract violation (rest-api.md §5.4), so the
	// pair travels together.
	"status",
	"gearPosition",
	// Speed / GPS atomic group — the entire point of a shared live map.
	"speed",
	"heading",
	"latitude",
	"longitude",
	"locationName",
	"locationAddress",
	// Service window and the ride-sharing switch. Both are operational
	// availability of the car, and the viewer is the party they are ABOUT: a
	// rider who cannot see them discovers a paused car from a 409 after
	// composing a whole trip request.
	"serviceEstimatedEndAt",
	"rideShareEnabled",
	// MYR-491 setup state — operational state about the car, listed EXPLICITLY
	// (this arm subtracts nothing by derivation any more) because a viewer
	// looking at a shared car that has never streamed must read "still being
	// set up" rather than a bare "offline" they can do nothing with.
	"setupState",
	// Odometer / FSD lifetime counters. Kept: these are neither media, cabin,
	// nor a control tile, so MYR-435 does not reach them, and `odometerMiles` /
	// `fsdMilesSinceReset` are both `required` in vehicle-state.schema.json.
	"odometerMiles",
	"fsdMilesSinceReset",
	// Navigation atomic group — "where is this car taking me" is the other half
	// of the shared-viewing feature (FR-5.1).
	"destinationName",
	"destinationAddress",
	"destinationLatitude",
	"destinationLongitude",
	"originLatitude",
	"originLongitude",
	"etaMinutes",
	"tripDistanceRemaining",
	"navRouteCoordinates",
	// Snapshot-aliased forms of the same navigation group (rest-api.md §7.1).
	"navDestinationName",
	"navDestinationLocation",
	"navOriginLocation",
	"navEtaMinutes",
	"navTripDistanceRemaining",
	// The accumulated live GPS trail behind the car on the map. Distinct from
	// the DRIVES resource, which MYR-369 made owner-only — this is the live
	// polyline of the trip the viewer is watching or sitting in, not the
	// vehicle's stored history.
	"driveTrailCoordinates",
	// Wire freshness marker. A viewer must be able to tell live from stale.
	"lastUpdated",
}

// vehicleStateOwnerOnlyFields enumerates every owner field that MYR-435 (and
// MYR-279 before it, for `vin`) withholds from viewers. It exists so the
// removals are STATED rather than inferred from the gap between two lists, and
// so TestVehicleStateRoleListsPartitionOwnerFields can prove the two lists
// together account for the owner list exactly — no field silently unclassified,
// no name here that no longer exists upstream.
//
// Client decision (2026-08-02): "Remove media/cabin and any vehicle controls."
var vehicleStateOwnerOnlyFields = []string{
	// MYR-279 — the full 17-char VIN. Party-scoped: it identifies the physical
	// car and links to its location history (data-classification.md §1.3,
	// §2.1). Predates MYR-435 and is unchanged by it.
	"vin",

	// --- Cabin climate (MYR-435) ------------------------------------------
	// The cabin is the owner's private space. Interior temperature is the
	// sharpest case after media: it is a live occupancy signal — it says
	// someone has been sitting in that car with the heat on.
	"interiorTemp",
	// `exteriorTemp` is the AMBIGUOUS one, and it is removed deliberately.
	// The argument to keep it: ambient air outside the car is not cabin
	// state and reveals nothing about the occupant — it is closer to weather
	// than to privacy. The argument that wins: a rider standing next to the
	// car already knows the weather, no viewer-facing surface renders it, and
	// it is delivered by the same climate read-back as its cabin siblings —
	// so for a viewer it is data with no consumer. Per the issue: default to
	// REMOVE when in doubt. (It is NOT in a declared x-atomic-group, so
	// removing it splits nothing — checked against vehicle-state.schema.json.)
	"exteriorTemp",
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
	// `seatCoolingCapable` is an equipment fact, which by the MYR-320 sibling
	// rule would sit with trim/model. It is removed anyway: its ONLY consumer
	// is the owner's seat-cooling control tile, deciding whether to draw the
	// control at all. With the seat controls gone for viewers, the capability
	// flag is a dangling dependency of a UI a viewer cannot reach.
	"seatCoolingCapable",

	// --- Vehicle controls state (MYR-435) ---------------------------------
	// Everything that exists to drive the OWNER's control tiles. A viewer
	// cannot actuate any of these (commands are owner-only at the routing
	// layer), so the state exists purely to render a control they do not
	// have — while telling them whether the owner's car is standing
	// unlocked, and whether its trunk is open.
	"locked",
	"chargePortDoorOpen",
	"frunkOpen",
	"trunkOpen",
	// `virtualKeyPaired` is the pairing flag the owner app reads to decide
	// whether commands can be sent at all. It is controls INFRASTRUCTURE: a
	// viewer sends no commands, so it is the same dangling dependency as
	// `seatCoolingCapable`.
	"virtualKeyPaired",

	// --- Media / now-playing (MYR-435) ------------------------------------
	// The audit's sharpest example. The five free-text fields are P1 USER
	// CONTENT, and an accumulated stream of them reveals listening habits
	// (taste, and by inference language, mood, politics, religion).
	//
	// The previous justification for showing them to viewers was "a rider in
	// the car can already hear it." That reasoning is now retired, and it is
	// worth naming WHY rather than leaving it to be rediscovered: a viewer is
	// not necessarily a rider. A share grant is durable and remote — it keeps
	// streaming while the grantee is at home and the owner is driving alone.
	// The "they can already hear it" defense only holds for the minutes
	// someone is actually in the passenger seat, and the mask cannot tell the
	// difference. The standing FR-5.5 note that these MUST be excluded from a
	// future `limited_viewer` tier is thereby resolved early and for every
	// viewer: this IS that exclusion.
	"mediaNowPlayingTitle",
	"mediaNowPlayingArtist",
	"mediaNowPlayingAlbum",
	"mediaNowPlayingStation",
	"mediaPlaybackSource",
	// The numerics are individually P0 — a bare track length or volume
	// ceiling identifies nothing. They go with the block anyway: elapsed and
	// duration together are a live "someone is listening right now, and is
	// 2:14 into it" signal, and playback status plus volume is the same
	// occupancy tell. The client said media, not "the identifying half of
	// media."
	"mediaPlaybackStatus",
	"mediaVolume",
	"mediaVolumeMax",
	"mediaNowPlayingDurationMs",
	"mediaNowPlayingElapsedMs",
}

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
	// MYR-491 — the setup state of a car that is not yet streaming, the same
	// object and the same derivation as VehicleState.setupState (§7.1). P0, and
	// NOT owner-private: this is the field the rider-side picker needs in order
	// to stop calling a never-streamed shared car "offline" (MYR-437), and the
	// picker reads catalog rows, not snapshots. The viewer list below inherits
	// it (that list subtracts nothing).
	"setupState",
	// MYR-507 — the display-safe trim label. P0, and NOT owner-private, for the
	// simplest reason on this list: `trimLabel` is an EQUIPMENT FACT of exactly
	// the same tier as its identity siblings `model`, `year` and `color`, which
	// have been in both allow-lists since v1. All four are legible through a
	// windshield from the kerb — the badge on the boot lid IS the trim — so
	// there is nothing here for masking to protect, and this entry classifies it
	// alongside its siblings rather than reasoning about it afresh. That is the
	// same argument `internal/mask/tables_details_test.go` already makes for the
	// SAME FIELD on the /snapshot resource, where it likewise goes to BOTH roles.
	//
	// The viewer is in fact the party this field is FOR: an owner can read the
	// trim off their own /snapshot, but a rider never fetches one, so the
	// catalog row is the only place a shared car can say what it is. Withholding
	// it is what left "UltraRed" standing in for a whole vehicle descriptor.
	//
	// `trim` — the RAW BADGE CODE ("p74d") — is deliberately NOT here, and is not
	// on the catalog at all: it is not display-safe for either role.
	"trimLabel",
	// MYR-515 — the car's last known position, and the FIRST P1 field on this
	// resource. Every other entry above is P0 except `licensePlate`, so this one
	// carries the heaviest justification on the list.
	//
	// WHY IT IS IN BOTH ROLE ALLOW-LISTS. The viewer already receives
	// `latitude`/`longitude` at FULL PRECISION on the streaming path, for
	// exactly these vehicles: `vehicleStateViewerFields` retains the whole
	// Speed/GPS atomic group, annotated there as "the entire point of a shared
	// live map", and MYR-435's narrowing deliberately left it. So a coordinate
	// on a catalog row is not a new disclosure — it is the same value, over a
	// different transport, to a party already entitled to it.
	//
	// The honest caveat, stated rather than glossed: the catalog does hand a
	// viewer N positions at once, where the socket hands out one per
	// subscription. That is an AGGREGATION difference, not an entitlement one —
	// the grant is per-vehicle and a viewer could subscribe to each car in turn
	// for the same data — so what this changes is convenience, not reach. The
	// row still only exists because a live accepted grant produced it: the
	// shared-catalog query's join IS the access check, and a suspended or
	// revoked grant yields no row and therefore no coordinate.
	//
	// ON COARSENING. Considered and deliberately NOT applied. The repo has no
	// precision convention to follow, and inventing one here would be theatre:
	// the same viewer can read the exact position off the socket a moment later,
	// so rounding the catalog protects nothing while measurably degrading the
	// feature it exists for — a pickup ETA over a short hop is exactly where a
	// coarsened coordinate is worst. If a future policy DOES coarsen shared
	// positions, it must coarsen the stream and this row together; splitting
	// them would leave the weaker surface setting the real privacy bound.
	//
	// P1 handling still binds in full: never logged (data-classification.md
	// §2.2), never emitted outside the vehicle's party, encrypted at rest, and
	// NOT on any WebSocket delta for this resource. And the wire value is
	// null-when-absent rather than the (0,0) sentinel — a sentinel a consumer
	// forgot to re-interpret is a coordinate leak of a different kind.
	"location",
	// MYR-581 — the owner's FIRST NAME, and the SECOND P1 field on this
	// resource after `location` (third counting `licensePlate`).
	//
	// WHY IT IS IN BOTH ROLE ALLOW-LISTS. The viewer is the party this field
	// exists for, and the disclosure is one the platform has already made to
	// exactly this party on other surfaces: `RedeemShareInviteResponse.
	// ownerFirstName` hands the redeeming viewer this same owner's first name at
	// the moment they join, the signed share link carries it as `from`, and the
	// push payloads carry a first name in both directions. A viewer holding a
	// live accepted grant on a car has already been told whose car it is; the
	// catalog row is where they can still see it a week later. Withholding it is
	// what left an incoming ride card reading "Tesla wants a ride" about a
	// person (MYR-532 item 4).
	//
	// The owner's own row naming the owner is a non-disclosure — they are the
	// subject — so it stays in the base list rather than becoming a viewer-only
	// entry; that also keeps one projection for all three §7.0 producers.
	//
	// FIRST NAMES ONLY, enforced upstream in the store's first-token reduction
	// rather than here. That is the platform's P1 counterparty policy, and it is
	// the reason this entry is not a full-name field: a mask cannot shorten a
	// value, only drop it, so the narrowing has to happen before the map is
	// built. What reaches this allow-list is already "Amruth", never "Amruth
	// Kelkar".
	//
	// P1 handling binds in full: NEVER logged (data-classification.md §2.2),
	// never emitted outside the vehicle's party (the shared-catalog query's join
	// IS the access check, so a suspended or revoked grant yields no row and
	// therefore no name), and NOT on any WebSocket delta for this resource.
	// `null` — the owner has no resolvable name — is the ordinary state for an
	// Apple-native account that was never asked, and is deliberately not the
	// empty string.
	"ownerFirstName",
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
