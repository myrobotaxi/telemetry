package store

import (
	"encoding/json"
	"time"
)

// jsonRawPtr returns a pointer to a json.RawMessage. Used for building
// VehicleUpdate fields from freshly marshalled JSON.
func jsonRawPtr(b json.RawMessage) *json.RawMessage { return &b }

// VehicleStatus mirrors the Prisma "VehicleStatus" enum stored in PostgreSQL.
type VehicleStatus string

const (
	VehicleStatusDriving   VehicleStatus = "driving"
	VehicleStatusParked    VehicleStatus = "parked"
	VehicleStatusCharging  VehicleStatus = "charging"
	VehicleStatusOffline   VehicleStatus = "offline"
	VehicleStatusInService VehicleStatus = "in_service"
)

// Vehicle is a read model of the Prisma "Vehicle" table. Only the fields
// the telemetry server needs are included.
type Vehicle struct {
	ID                   string
	UserID               string
	VIN                  string
	Name                 string
	Model                string
	Year                 int
	Color                string
	Status               VehicleStatus
	ChargeLevel          int
	EstimatedRange       int
	ChargeState          *string  // nullable; Tesla proto 179 enum string
	TimeToFull           *float64 // nullable; Tesla proto 43, hours (decimal)
	Speed                int
	GearPosition         *string // nullable
	Heading              int
	Latitude             float64
	Longitude            float64
	LocationName         string
	LocationAddress      string
	InteriorTemp         int
	ExteriorTemp         int
	OdometerMiles        int
	FsdMilesSinceReset   float64
	DestinationName      *string         // nullable
	DestinationAddress   *string         // nullable
	DestinationLatitude  *float64        // nullable
	DestinationLongitude *float64        // nullable
	OriginLatitude       *float64        // nullable
	OriginLongitude      *float64        // nullable
	EtaMinutes           *int            // nullable
	TripDistRemaining    *float64        // nullable
	NavRouteCoordinates  json.RawMessage // nullable JSONB
	LastUpdated          time.Time
}

// VehicleUpdate holds the subset of vehicle fields that can change from
// a single telemetry event. Nil pointer fields are not written to the
// database, allowing partial updates.
type VehicleUpdate struct {
	Speed                *int
	ChargeLevel          *int
	EstimatedRange       *int
	ChargeState          *string  // Tesla proto 179 enum string
	TimeToFull           *float64 // Tesla proto 43, hours (decimal)
	GearPosition         *string
	Heading              *int
	Latitude             *float64
	Longitude            *float64
	InteriorTemp         *int
	ExteriorTemp         *int
	OdometerMiles        *int
	FsdMilesSinceReset   *float64
	LocationName         *string
	LocationAddr         *string
	DestinationName      *string
	DestinationAddress   *string
	DestinationLatitude  *float64
	DestinationLongitude *float64
	OriginLatitude       *float64
	OriginLongitude      *float64
	EtaMinutes           *int
	TripDistRemaining    *float64
	NavRouteCoordinates  *json.RawMessage // [lng, lat] pairs as JSON array
	ClearFields          []string         // DB column names to explicitly SET NULL
	LastUpdated          time.Time        // always set
}

// DriveRecord maps to the Prisma "Drive" table.
type DriveRecord struct {
	ID               string
	VehicleID        string
	Date             string // "2026-03-17" -- matches Prisma's String type
	StartTime        string // ISO 8601 -- matches Prisma's String type
	EndTime          string
	StartLocation    string
	StartAddress     string
	EndLocation      string
	EndAddress       string
	DistanceMiles    float64
	DurationMinutes  int
	AvgSpeedMph      float64
	MaxSpeedMph      float64
	EnergyUsedKwh    float64
	StartChargeLevel int
	EndChargeLevel   int
	FsdMiles         float64
	FsdPercentage    float64
	Interventions    int
	RoutePoints      json.RawMessage // JSONB -- pass through as raw JSON
	CreatedAt        time.Time
}

// DriveCompletion holds the final values written when a drive ends.
type DriveCompletion struct {
	EndTime         string
	EndLocation     string
	EndAddress      string
	DistanceMiles   float64
	DurationMinutes int
	AvgSpeedMph     float64
	MaxSpeedMph     float64
	EnergyUsedKwh   float64
	EndChargeLevel  int
	FsdMiles        float64
	FsdPercentage   float64
	Interventions   int
}

// RoutePointRecord is a single GPS point stored inside the Drive.routePoints
// JSONB array. Matches the existing JSON format used by the Next.js app.
type RoutePointRecord struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lng"`
	Speed     float64 `json:"speed"`
	Heading   float64 `json:"heading"`
	Timestamp string  `json:"timestamp"` // ISO 8601
}

// TeslaOAuthToken holds the Tesla OAuth2 credentials read from the
// Prisma-owned Account table. Read-only — never modify this table.
type TeslaOAuthToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    *int64 // Unix epoch seconds, nullable
}
