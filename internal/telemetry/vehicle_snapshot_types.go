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
	return m
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
	}
}
