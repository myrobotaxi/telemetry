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
	ID                   string
	UserID               string
	VIN                  string
	Name                 string
	Model                string
	Year                 int
	Color                string
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
	VehicleID            string          `json:"vehicleId"`
	Name                 string          `json:"name"`
	Model                string          `json:"model"`
	Year                 int             `json:"year"`
	Color                string          `json:"color"`
	// VIN (MYR-279): the FULL 17-char VIN, owner-snapshot only. Gated to the
	// owner mask (never viewer, never WS broadcast); the vehicles-list keeps
	// vinLast4. See docs/contracts/data-classification.md section 1.3.
	VIN string `json:"vin"`
	// SoftwareVersion / Trim (MYR-279): nullable vehicle-detail read-backs.
	SoftwareVersion      *string         `json:"softwareVersion"`
	Trim                 *string         `json:"trim"`
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
	m["vin"] = r.VIN
	m["softwareVersion"] = derefOrNil(r.SoftwareVersion)
	m["trim"] = derefOrNil(r.Trim)
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
}

// buildSnapshotResponse maps the store-layer row into the wire shape.
// Time formatting matches rest-api.md §7.1's RFC 3339 example.
func buildSnapshotResponse(row VehicleSnapshotRow) vehicleSnapshotResponse {
	return vehicleSnapshotResponse{
		VehicleID:            row.ID,
		Name:                 row.Name,
		Model:                row.Model,
		Year:                 row.Year,
		Color:                row.Color,
		VIN:                  row.VIN,
		SoftwareVersion:      row.SoftwareVersion,
		Trim:                 row.Trim,
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
	}
}
