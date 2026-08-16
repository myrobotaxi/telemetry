package telemetry

import (
	"context"
	"encoding/json"
	"time"
)

// VehicleSnapshotRow is the per-vehicle shape the snapshot handler
// consumes from its VehicleSnapshotReader dependency. Mirrors every
// field in docs/contracts/schemas/vehicle-state.schema.json that the
// Go server is responsible for (the full v1 VehicleState shape). The
// adapter in `cmd/telemetry-server` wires `store.VehicleRepo.GetByID`
// into this interface and converts `store.Vehicle` → VehicleSnapshotRow
// at the boundary so the handler stays decoupled from `internal/store`.
type VehicleSnapshotRow struct {
	ID     string
	UserID string
	VIN    string
	Name   string
	Model  string
	Year   int
	Color  string
	// LicensePlate is the owner-entered plate (MYR-286), an IDENTITY-row
	// field read straight off the Prisma "Vehicle" column like Color/Name —
	// not telemetry, never streamed. Empty string == not set.
	LicensePlate         string
	Status               string
	ChargeLevel          int
	EstimatedRange       int
	ChargeState          *string
	TimeToFull           *float64
	Speed                int
	GearPosition         *string
	Heading              int
	Latitude             float64
	Longitude            float64
	LocationName         string
	LocationAddress      string
	InteriorTemp         int
	ExteriorTemp         int
	OdometerMiles        int
	FsdMilesSinceReset   float64
	DestinationName      *string
	DestinationAddress   *string
	DestinationLatitude  *float64
	DestinationLongitude *float64
	OriginLatitude       *float64
	OriginLongitude      *float64
	EtaMinutes           *int
	TripDistRemaining    *float64
	NavRouteCoordinates  json.RawMessage
	LastUpdated          time.Time

	// MYR-269 / MYR-273 owner-control read-backs, hydrated from the
	// go_vehicle_control_state side table on the snapshot read path. Nullable —
	// nil means never read.
	Locked             *bool
	FrunkOpen          *bool
	TrunkOpen          *bool
	IsClimateOn        *bool
	ChargePortDoorOpen *bool

	// MYR-273 cabin-setting levels.
	DriverTempSetting    *int
	PassengerTempSetting *int
	FanSpeed             *int
	SeatHeaterLeft       *int
	SeatHeaterRight      *int
	SeatHeaterRearLeft   *int
	SeatHeaterRearCenter *int
	SeatHeaterRearRight  *int
	SeatCoolerLeft       *int
	SeatCoolerRight      *int
	MediaVolume          *float64

	// MYR-279 vehicle-detail read-backs (software version + trim), same
	// go_vehicle_control_state side table, same GetByID LEFT JOIN. Nullable.
	SoftwareVersion *string
	Trim            *string

	// MYR-274 climate-mode read-backs (hvac auto mode string, A/C enabled bool),
	// same side table, same GetByID LEFT JOIN. Nullable — nil means never read.
	HvacAutoMode  *string
	HvacAcEnabled *bool

	// MYR-298 seat-ventilation + media-playback read-backs, same side table,
	// same GetByID LEFT JOIN. Nullable — nil means never read.
	SeatVentEnabled     *bool
	MediaPlaybackStatus *string

	// MYR-303 media now-playing block, same side table, same LEFT JOIN. Nullable
	// — nil means NEVER OBSERVED. For the five text fields that is distinct from
	// a non-nil EMPTY string, which means "observed, and nothing is playing".
	MediaNowPlayingTitle    *string
	MediaNowPlayingArtist   *string
	MediaNowPlayingAlbum    *string
	MediaNowPlayingStation  *string
	MediaPlaybackSource     *string
	MediaNowPlayingDuration *int64
	MediaNowPlayingElapsed  *int64
	MediaVolumeMax          *float64

	// MYR-308 ventilated-seat capability, same side table, same LEFT JOIN.
	// Nullable — nil means never read, NOT "no seat cooling".
	SeatCoolingCapable *bool

	// MYR-316 service window: the two RAW sources behind the single wire field
	// serviceEstimatedEndAt, same side table, same LEFT JOIN. ServiceETC is
	// Tesla's own estimate and takes precedence; ServiceExpectedEndAt is the
	// owner-entered fallback. The wire value is resolved from these plus
	// Status by resolveServiceEstimatedEndAt — never emitted raw.
	ServiceETC           *time.Time
	ServiceExpectedEndAt *time.Time

	// MYR-320 vehicle-detail read-backs, same side table, same LEFT JOIN.
	// TrimLabel is the DISPLAY-SAFE trim label and stands alongside Trim above
	// (the raw badge code); FSDVersion is the FSD software designation, a
	// different value from SoftwareVersion above (the firmware build). Nullable
	// — nil means never read.
	TrimLabel  *string
	FSDVersion *string

	// MYR-342 owner ride-sharing switch, same side table, same LEFT JOIN. NOT a
	// pointer: the column is NOT NULL and the store's COALESCE turns a missing
	// side-table row into true, so there is no "never read" state — a car nobody
	// has paused is accepting rides.
	//
	// This field is also the READ THE TWO REQUEST-TIME GATES USE. Both the
	// ride-request create gate and the owner accept backstop already call
	// GetByID to establish ownership / status, so taking the pause off the SAME
	// row means the two facts cannot disagree — there is no window in which a
	// caller is authorised against one snapshot of the vehicle and gated against
	// another.
	RideShareEnabled bool

	// MYR-491 fleet-config setup schedule, LEFT JOINed from the Go-owned
	// go_fleet_config_attempts table (a SECOND side table on this read, not the
	// control-state one above). RAW STORAGE: the wire field `setupState` is
	// DERIVED from it by deriveSetupState together with Status and LastUpdated,
	// and is never emitted raw — the same discipline as the ServiceETC pair.
	SetupSchedule VehicleSetupSchedule
}

// VehicleSnapshotReader returns the snapshot row for a Prisma cuid.
// Implementations should return an error wrapping sdk.ErrNotFound when
// the vehicleID is unknown.
type VehicleSnapshotReader interface {
	GetByID(ctx context.Context, vehicleID string) (VehicleSnapshotRow, error)
}

// vehicleSnapshotResponse is the wire shape returned by the snapshot
// endpoint. JSON tags mirror docs/contracts/schemas/vehicle-state.schema.json
// and the per-role allow-list in `internal/mask/tables.go`
// (vehicleStateOwnerFields). The struct is dehydrated into a
// map[string]any by toMaskMap before projection so the mask layer can
// strip denied keys without touching the source struct.
//
// Nullable fields use *T and are flattened to a typed-nil JSON value
// after projection — matches the "absent, not nulled" rule in
// rest-api.md §5.1 for denied fields but preserves null on the wire
// for permitted nullable fields (the schema marks these explicitly
// nullable).
type vehicleSnapshotResponse struct {
	VehicleID string `json:"vehicleId"`
	Name      string `json:"name"`
	Model     string `json:"model"`
	Year      int    `json:"year"`
	Color     string `json:"color"`
	// LicensePlate (MYR-286): the owner-entered plate. Emitted with the SAME
	// convention as its sibling identity-row field `color` above — a plain
	// string with NO omitempty, so the key is ALWAYS present and "not set" is
	// an empty string rather than a missing key. (The wire contract tolerates
	// both absent and "" as "no plate set"; this server always sends "".)
	// Visible to BOTH roles by deliberate product decision (MYR-286) — riders
	// identify the car at pickup — unlike the owner-only `vin` below.
	LicensePlate string `json:"licensePlate"`
	// VIN (MYR-279): the FULL 17-char VIN, owner-snapshot only. Gated to the
	// owner mask (never viewer, never WS broadcast); the vehicles-list keeps
	// vinLast4. See docs/contracts/data-classification.md section 1.3.
	VIN string `json:"vin"`
	// SoftwareVersion / Trim (MYR-279): nullable vehicle-detail read-backs.
	SoftwareVersion *string `json:"softwareVersion"`
	Trim            *string `json:"trim"`
	// TrimLabel / FSDVersion (MYR-320): nullable vehicle-detail read-backs,
	// SNAPSHOT-ONLY — REST-derived, so a WS vehicle_update frame never carries
	// either. TrimLabel is the display-safe sibling of Trim (which stays the raw
	// badge code and must not be rendered); FSDVersion is passed through
	// VERBATIM and is distinct from SoftwareVersion (the firmware build).
	TrimLabel            *string         `json:"trimLabel"`
	FSDVersion           *string         `json:"fsdVersion"`
	Status               string          `json:"status"`
	Speed                int             `json:"speed"`
	Heading              int             `json:"heading"`
	Latitude             float64         `json:"latitude"`
	Longitude            float64         `json:"longitude"`
	LocationName         string          `json:"locationName"`
	LocationAddress      string          `json:"locationAddress"`
	GearPosition         *string         `json:"gearPosition"`
	ChargeLevel          int             `json:"chargeLevel"`
	ChargeState          *string         `json:"chargeState"`
	EstimatedRange       int             `json:"estimatedRange"`
	TimeToFull           *float64        `json:"timeToFull"`
	InteriorTemp         int             `json:"interiorTemp"`
	ExteriorTemp         int             `json:"exteriorTemp"`
	OdometerMiles        int             `json:"odometerMiles"`
	FsdMilesSinceReset   float64         `json:"fsdMilesSinceReset"`
	DestinationName      *string         `json:"destinationName"`
	DestinationAddress   *string         `json:"destinationAddress"`
	DestinationLatitude  *float64        `json:"destinationLatitude"`
	DestinationLongitude *float64        `json:"destinationLongitude"`
	OriginLatitude       *float64        `json:"originLatitude"`
	OriginLongitude      *float64        `json:"originLongitude"`
	EtaMinutes           *int            `json:"etaMinutes"`
	TripDistRemaining    *float64        `json:"tripDistanceRemaining"`
	NavRouteCoordinates  json.RawMessage `json:"navRouteCoordinates"`
	LastUpdated          string          `json:"lastUpdated"`

	// MYR-269: owner-control read-backs, now persisted (go_vehicle_control_state)
	// and returned on the DB-backed /snapshot for non-streaming cars. Wire names
	// match the live WS vehicle_update fields (internal/ws/field_mapping.go,
	// door_fields.go) so the client's Vehicle model reconciles REST and WS. All
	// nullable: null == never read (honest "unavailable"), never a fabricated
	// on/off. On the owner mask allow-list (internal/mask/tables.go).
	Locked             *bool `json:"locked"`
	FrunkOpen          *bool `json:"frunkOpen"`
	TrunkOpen          *bool `json:"trunkOpen"`
	IsClimateOn        *bool `json:"isClimateOn"`
	ChargePortDoorOpen *bool `json:"chargePortDoorOpen"`

	// MYR-273: cabin-setting levels, now persisted (go_vehicle_control_state) and
	// returned on the DB-backed /snapshot for non-streaming cars. Wire names match
	// the live WS vehicle_update fields (internal/ws/field_mapping.go) so the
	// client's Vehicle model reconciles REST and WS. All nullable: null == never
	// read (honest "—"). On the owner mask allow-list (internal/mask/tables.go).
	DriverTempSetting    *int     `json:"driverTempSetting"`
	PassengerTempSetting *int     `json:"passengerTempSetting"`
	FanSpeed             *int     `json:"fanSpeed"`
	SeatHeaterLeft       *int     `json:"seatHeaterLeft"`
	SeatHeaterRight      *int     `json:"seatHeaterRight"`
	SeatHeaterRearLeft   *int     `json:"seatHeaterRearLeft"`
	SeatHeaterRearCenter *int     `json:"seatHeaterRearCenter"`
	SeatHeaterRearRight  *int     `json:"seatHeaterRearRight"`
	SeatCoolerLeft       *int     `json:"seatCoolerLeft"`
	SeatCoolerRight      *int     `json:"seatCoolerRight"`
	MediaVolume          *float64 `json:"mediaVolume"`

	// MYR-274: climate-MODE read-backs backing the owner Auto/Cool/Heat segment,
	// now persisted (go_vehicle_control_state) and returned on the DB-backed
	// /snapshot for non-streaming cars. Wire names match the live WS vehicle_update
	// fields (internal/ws/field_mapping.go — both pass through unchanged) so the
	// client's Vehicle model reconciles REST and WS. Nullable: null == never read
	// (honest-unknown — the segment stays unresolved), never a fabricated mode. On
	// the owner mask allow-list (internal/mask/tables.go, since MYR-252).
	HvacAutoMode  *string `json:"hvacAutoMode"`
	HvacAcEnabled *bool   `json:"hvacAcEnabled"`

	// MYR-298: the last two MYR-252 cabin read-backs that were live-WS-only, now
	// persisted (go_vehicle_control_state) and returned on the DB-backed
	// /snapshot so a client that missed the live vehicle_update frame can still
	// learn them. Wire names match the live WS fields (internal/ws/
	// field_mapping.go — both pass through unchanged) so the client's Vehicle
	// model reconciles REST and WS. Nullable: null == never read (honest-unknown),
	// never a fabricated value. On the owner mask allow-list (internal/mask/
	// tables.go, since MYR-252).
	SeatVentEnabled     *bool   `json:"seatVentEnabled"`
	MediaPlaybackStatus *string `json:"mediaPlaybackStatus"`

	// MYR-303: the media NOW-PLAYING block, persisted (go_vehicle_control_state)
	// and returned on the DB-backed /snapshot so a car that is asleep, offline or
	// in service still surfaces the last-known track instead of an empty panel.
	// Wire names match the live WS vehicle_update fields (they pass through
	// internal/ws unchanged) so the client's Vehicle model reconciles REST and WS.
	//
	// Nullable, and the null carries a SPECIFIC meaning for the five text
	// fields: null == never observed, whereas an empty string == observed and
	// nothing is playing. Clients must not collapse the two — the first is
	// "we don't know", the second is "we know, and it's nothing".
	MediaNowPlayingTitle    *string  `json:"mediaNowPlayingTitle"`
	MediaNowPlayingArtist   *string  `json:"mediaNowPlayingArtist"`
	MediaNowPlayingAlbum    *string  `json:"mediaNowPlayingAlbum"`
	MediaNowPlayingStation  *string  `json:"mediaNowPlayingStation"`
	MediaPlaybackSource     *string  `json:"mediaPlaybackSource"`
	MediaNowPlayingDuration *int64   `json:"mediaNowPlayingDurationMs"`
	MediaNowPlayingElapsed  *int64   `json:"mediaNowPlayingElapsedMs"`
	MediaVolumeMax          *float64 `json:"mediaVolumeMax"`

	// MYR-308: the ventilated-seat CAPABILITY (a spec fact from REST
	// vehicle_config, not a runtime state — contrast seatVentEnabled above).
	// SNAPSHOT-ONLY by construction: Tesla does not stream it, so it never
	// appears on a WS vehicle_update frame. Nullable: null == the server has
	// never completed a vehicle-config read for this car, which clients MUST
	// treat as "unknown, fall back to the seatCooler*-presence heuristic" and
	// NOT as "no seat cooling". An explicit false is the authoritative no.
	SeatCoolingCapable *bool `json:"seatCoolingCapable"`

	// ServiceEstimatedEndAt is when the car's CURRENT SERVICE VISIT is expected
	// to end (MYR-316, contracts v0.17.0). RFC 3339 UTC, or null.
	//
	// SERVER-COMPUTED with a fixed precedence: Tesla's own `service_etc` from
	// the Fleet API service_data endpoint, else the owner-entered value from
	// §7.16, else null. Tesla returns an all-null service_data body for a visit
	// with no appointment record, so a null here is COMMON AND NORMAL — not an
	// error, not a fetch failure, and never a claim that the car is back.
	//
	// Meaningful ONLY while the sibling `status` is `in_service`, and null
	// otherwise: consumers never have to age this value out themselves.
	//
	// SNAPSHOT-ONLY: REST-derived, never streamed — a WS vehicle_update frame
	// NEVER carries it.
	ServiceEstimatedEndAt *string `json:"serviceEstimatedEndAt"`

	// RideShareEnabled is the owner's ride-sharing switch (MYR-342,
	// contracts v0.20.0) — the same value and the same semantics as
	// VehicleSummary.rideShareEnabled on the catalog row.
	//
	// Carried HERE as well as on the list row on purpose: the owner's toggle
	// lives on the vehicle detail surface, and a control whose current position
	// can only be learned from a different endpoint is a control that renders
	// wrong on a cold open. (Contrast `hasActiveRide`, which is list-only —
	// nothing on the detail sheet reads it.)
	//
	// NOT a pointer and NO omitempty: `false` is a real, load-bearing value —
	// the wire contract reads an ABSENT key as ENABLED, so an omitted false
	// would un-pause a paused car on every read.
	//
	// SNAPSHOT-ONLY: owner intent, not telemetry — a WS vehicle_update frame
	// NEVER carries it, and no Tesla field feeds it.
	RideShareEnabled bool `json:"rideShareEnabled"`

	// SetupState names the ONE thing still standing between this car and live
	// telemetry, or is null when there is nothing to say (MYR-491,
	// contracts v0.24.0).
	//
	// A POINTER WITH NO omitempty, so the key is ALWAYS present and "nothing to
	// finish" is an explicit `null` — the same honest-unknown convention as
	// serviceEstimatedEndAt. There is deliberately no `ready` member: a client
	// handed one would badge a car green off a value that really means "no
	// claim", which is also what a pre-MYR-491 server emits by omitting the key.
	//
	// SERVER-DERIVED, exactly once, in deriveSetupState. No client re-derives
	// it, and both read surfaces call the same function with the same inputs so
	// the catalog row and the snapshot cannot disagree.
	//
	// SNAPSHOT/LIST-ONLY: a WS vehicle_update frame NEVER carries it. Its
	// inputs change on reconciler passes and owner actions, not on telemetry
	// frames — and a car in any of these states is by definition not sending
	// frames to attach it to.
	SetupState *SetupState `json:"setupState"`
}

// toMaskMap returns the response as a wire-name-keyed map suitable for
// projection through the role-based mask. Mirrors the pattern in
// vehicle_status_handler.go ToMaskMap and vehicles_list_handler.go.
// Pointer fields are flattened to their pointed-to value or nil so the
// mask matrix's allow-list (which is keyed by JSON name) sees the same
// key set the encoder will emit.
func (r vehicleSnapshotResponse) toMaskMap() map[string]any {
	m := make(map[string]any, 32)
	m["vehicleId"] = r.VehicleID
	m["name"] = r.Name
	m["model"] = r.Model
	m["year"] = r.Year
	m["color"] = r.Color
	// Always keyed (never conditional on emptiness) — same as `color`.
	m["licensePlate"] = r.LicensePlate
	m["vin"] = r.VIN
	m["softwareVersion"] = derefOrNil(r.SoftwareVersion)
	m["trim"] = derefOrNil(r.Trim)
	// MYR-320 — REST-sourced, snapshot-only (never on a WS vehicle_update).
	m["trimLabel"] = derefOrNil(r.TrimLabel)
	m["fsdVersion"] = derefOrNil(r.FSDVersion)
	m["status"] = r.Status
	m["speed"] = r.Speed
	m["heading"] = r.Heading
	m["latitude"] = r.Latitude
	m["longitude"] = r.Longitude
	m["locationName"] = r.LocationName
	m["locationAddress"] = r.LocationAddress
	m["gearPosition"] = derefOrNil(r.GearPosition)
	m["chargeLevel"] = r.ChargeLevel
	m["chargeState"] = derefOrNil(r.ChargeState)
	m["estimatedRange"] = r.EstimatedRange
	m["timeToFull"] = derefOrNil(r.TimeToFull)
	m["interiorTemp"] = r.InteriorTemp
	m["exteriorTemp"] = r.ExteriorTemp
	m["odometerMiles"] = r.OdometerMiles
	m["fsdMilesSinceReset"] = r.FsdMilesSinceReset
	m["destinationName"] = derefOrNil(r.DestinationName)
	m["destinationAddress"] = derefOrNil(r.DestinationAddress)
	m["destinationLatitude"] = derefOrNil(r.DestinationLatitude)
	m["destinationLongitude"] = derefOrNil(r.DestinationLongitude)
	m["originLatitude"] = derefOrNil(r.OriginLatitude)
	m["originLongitude"] = derefOrNil(r.OriginLongitude)
	m["etaMinutes"] = derefOrNil(r.EtaMinutes)
	m["tripDistanceRemaining"] = derefOrNil(r.TripDistRemaining)
	if len(r.NavRouteCoordinates) > 0 {
		m["navRouteCoordinates"] = r.NavRouteCoordinates
	} else {
		m["navRouteCoordinates"] = nil
	}
	m["lastUpdated"] = r.LastUpdated
	addSnapshotControlFields(m, r)
	return m
}

// addSnapshotControlFields adds the MYR-269 owner-control read-backs and the
// MYR-273 cabin-setting levels to the mask map, keyed by their live WS wire names
// so the owner mask allow-list (which already lists these from MYR-252) permits
// them. Split out of toMaskMap to keep that method under the funlen cap.
func addSnapshotControlFields(m map[string]any, r vehicleSnapshotResponse) {
	m["locked"] = derefOrNil(r.Locked)
	m["frunkOpen"] = derefOrNil(r.FrunkOpen)
	m["trunkOpen"] = derefOrNil(r.TrunkOpen)
	m["isClimateOn"] = derefOrNil(r.IsClimateOn)
	m["chargePortDoorOpen"] = derefOrNil(r.ChargePortDoorOpen)
	m["driverTempSetting"] = derefOrNil(r.DriverTempSetting)
	m["passengerTempSetting"] = derefOrNil(r.PassengerTempSetting)
	m["fanSpeed"] = derefOrNil(r.FanSpeed)
	m["seatHeaterLeft"] = derefOrNil(r.SeatHeaterLeft)
	m["seatHeaterRight"] = derefOrNil(r.SeatHeaterRight)
	m["seatHeaterRearLeft"] = derefOrNil(r.SeatHeaterRearLeft)
	m["seatHeaterRearCenter"] = derefOrNil(r.SeatHeaterRearCenter)
	m["seatHeaterRearRight"] = derefOrNil(r.SeatHeaterRearRight)
	m["seatCoolerLeft"] = derefOrNil(r.SeatCoolerLeft)
	m["seatCoolerRight"] = derefOrNil(r.SeatCoolerRight)
	m["mediaVolume"] = derefOrNil(r.MediaVolume)
	// MYR-274 climate-mode read-backs, keyed by the live WS wire names (on the
	// owner mask allow-list since MYR-252).
	m["hvacAutoMode"] = derefOrNil(r.HvacAutoMode)
	m["hvacAcEnabled"] = derefOrNil(r.HvacAcEnabled)
	// MYR-298 seat-ventilation + media-playback read-backs, keyed by the live WS
	// wire names (on the owner mask allow-list since MYR-252). Same COALESCE-fed
	// freshness semantics as the siblings above: the key is ALWAYS present, and a
	// never-read field is an explicit null rather than an omitted key or a
	// fabricated value.
	m["seatVentEnabled"] = derefOrNil(r.SeatVentEnabled)
	m["mediaPlaybackStatus"] = derefOrNil(r.MediaPlaybackStatus)
	addSnapshotMediaFields(m, r)
}

// addSnapshotMediaFields adds the MYR-303 media now-playing block and the
// MYR-308 ventilated-seat capability to the mask map, keyed by their wire names.
// Split from addSnapshotControlFields to keep both under the funlen cap.
//
// Absent-vs-null matches the siblings exactly: the key is ALWAYS written, so the
// wire always carries it and a never-observed field is an explicit JSON null
// rather than an omitted key or a fabricated value. For the five text fields the
// null is load-bearing and must NOT be collapsed with the empty string that
// derefOrNil passes through for an observed-but-cleared track: null == never
// observed, "" == nothing playing.
func addSnapshotMediaFields(m map[string]any, r vehicleSnapshotResponse) {
	m["mediaNowPlayingTitle"] = derefOrNil(r.MediaNowPlayingTitle)
	m["mediaNowPlayingArtist"] = derefOrNil(r.MediaNowPlayingArtist)
	m["mediaNowPlayingAlbum"] = derefOrNil(r.MediaNowPlayingAlbum)
	m["mediaNowPlayingStation"] = derefOrNil(r.MediaNowPlayingStation)
	m["mediaPlaybackSource"] = derefOrNil(r.MediaPlaybackSource)
	m["mediaNowPlayingDurationMs"] = derefOrNil(r.MediaNowPlayingDuration)
	m["mediaNowPlayingElapsedMs"] = derefOrNil(r.MediaNowPlayingElapsed)
	m["mediaVolumeMax"] = derefOrNil(r.MediaVolumeMax)
	// MYR-308 — REST-sourced, snapshot-only (never on a WS vehicle_update).
	m["seatCoolingCapable"] = derefOrNil(r.SeatCoolingCapable)
	// MYR-316 — already resolved (precedence + in-service gate) by
	// buildSnapshotResponse; this is the emitted value, not a raw column.
	m["serviceEstimatedEndAt"] = derefOrNil(r.ServiceEstimatedEndAt)
	// MYR-342 — the owner's switch, emitted raw (nothing to resolve). Keyed
	// unconditionally, and permitted for BOTH roles by the mask tables.
	m["rideShareEnabled"] = r.RideShareEnabled
	// MYR-491 — already derived by buildSnapshotResponse; this is the emitted
	// value. Keyed unconditionally (an explicit null when there is no claim) and
	// permitted for BOTH roles: a rider looking at a shared car needs to read
	// "setting up" rather than a bare "offline" (MYR-437).
	//
	// setupStateWire keeps the nil pointer out of the map as an untyped nil:
	// a typed (*SetupState)(nil) stored in an `any` is NOT equal to nil, and
	// downstream mask/JSON handling treats the two differently.
	m["setupState"] = setupStateWire(r.SetupState)
}

// setupStateWire normalises the optional setup state for the mask map. A nil
// *SetupState becomes an untyped nil so the key encodes as JSON `null` and
// compares equal to nil for any consumer that checks.
func setupStateWire(s *SetupState) any {
	if s == nil {
		return nil
	}
	return s
}

// buildSnapshotResponse maps the store-layer row into the wire shape.
// Time formatting matches rest-api.md §7.1's RFC 3339 example.
//
// now is passed rather than read here so the MYR-491 setup derivation — whose
// every rule is a comparison against the clock — is testable without waiting,
// and so one request cannot straddle two readings of the time.
func buildSnapshotResponse(row VehicleSnapshotRow, now time.Time) vehicleSnapshotResponse {
	return vehicleSnapshotResponse{
		VehicleID:       row.ID,
		Name:            row.Name,
		Model:           row.Model,
		Year:            row.Year,
		Color:           row.Color,
		LicensePlate:    row.LicensePlate,
		VIN:             row.VIN,
		SoftwareVersion: row.SoftwareVersion,
		Trim:            row.Trim,
		// MYR-578: the SAME resolver the catalog runs, over the same inputs —
		// Tesla's display-safe label, else the badge, else the VIN drive-unit —
		// so the detail sheet and the picker cannot name one car two ways. The
		// raw badge above stays raw (owner-snapshot-only, MYR-279's field).
		TrimLabel:            resolvedTrimLabel(row.Model, row.Year, row.TrimLabel, row.Trim, row.VIN),
		FSDVersion:           row.FSDVersion,
		Status:               row.Status,
		Speed:                row.Speed,
		Heading:              row.Heading,
		Latitude:             row.Latitude,
		Longitude:            row.Longitude,
		LocationName:         row.LocationName,
		LocationAddress:      row.LocationAddress,
		GearPosition:         row.GearPosition,
		ChargeLevel:          row.ChargeLevel,
		ChargeState:          row.ChargeState,
		EstimatedRange:       row.EstimatedRange,
		TimeToFull:           row.TimeToFull,
		InteriorTemp:         row.InteriorTemp,
		ExteriorTemp:         row.ExteriorTemp,
		OdometerMiles:        row.OdometerMiles,
		FsdMilesSinceReset:   row.FsdMilesSinceReset,
		DestinationName:      row.DestinationName,
		DestinationAddress:   row.DestinationAddress,
		DestinationLatitude:  row.DestinationLatitude,
		DestinationLongitude: row.DestinationLongitude,
		OriginLatitude:       row.OriginLatitude,
		OriginLongitude:      row.OriginLongitude,
		EtaMinutes:           row.EtaMinutes,
		TripDistRemaining:    row.TripDistRemaining,
		NavRouteCoordinates:  row.NavRouteCoordinates,
		LastUpdated:          row.LastUpdated.UTC().Format(time.RFC3339),
		Locked:               row.Locked,
		FrunkOpen:            row.FrunkOpen,
		TrunkOpen:            row.TrunkOpen,
		IsClimateOn:          row.IsClimateOn,
		ChargePortDoorOpen:   row.ChargePortDoorOpen,
		DriverTempSetting:    row.DriverTempSetting,
		PassengerTempSetting: row.PassengerTempSetting,
		FanSpeed:             row.FanSpeed,
		SeatHeaterLeft:       row.SeatHeaterLeft,
		SeatHeaterRight:      row.SeatHeaterRight,
		SeatHeaterRearLeft:   row.SeatHeaterRearLeft,
		SeatHeaterRearCenter: row.SeatHeaterRearCenter,
		SeatHeaterRearRight:  row.SeatHeaterRearRight,
		SeatCoolerLeft:       row.SeatCoolerLeft,
		SeatCoolerRight:      row.SeatCoolerRight,
		MediaVolume:          row.MediaVolume,
		HvacAutoMode:         row.HvacAutoMode,
		HvacAcEnabled:        row.HvacAcEnabled,
		SeatVentEnabled:      row.SeatVentEnabled,
		MediaPlaybackStatus:  row.MediaPlaybackStatus,

		MediaNowPlayingTitle:    row.MediaNowPlayingTitle,
		MediaNowPlayingArtist:   row.MediaNowPlayingArtist,
		MediaNowPlayingAlbum:    row.MediaNowPlayingAlbum,
		MediaNowPlayingStation:  row.MediaNowPlayingStation,
		MediaPlaybackSource:     row.MediaPlaybackSource,
		MediaNowPlayingDuration: row.MediaNowPlayingDuration,
		MediaNowPlayingElapsed:  row.MediaNowPlayingElapsed,
		MediaVolumeMax:          row.MediaVolumeMax,
		SeatCoolingCapable:      row.SeatCoolingCapable,
		// MYR-316: resolved here, so the precedence and the in-service gate are
		// applied exactly once per surface (service_window.go).
		ServiceEstimatedEndAt: serviceEstimatedEndAtWire(row.Status, row.ServiceETC, row.ServiceExpectedEndAt),
		// MYR-342: passed straight through — the owner's answer IS the wire
		// value, with no precedence to apply and no status to gate it on.
		RideShareEnabled: row.RideShareEnabled,
		// MYR-491: derived here, so the streaming gate and the expiry window
		// are applied exactly once per surface (vehicle_setup_state.go).
		SetupState: deriveSetupState(now, row.Status, row.LastUpdated, row.SetupSchedule),
	}
}
