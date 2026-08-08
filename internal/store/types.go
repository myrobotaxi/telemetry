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
	ID     string
	UserID string
	VIN    string
	Name   string
	Model  string
	Year   int
	Color  string
	// LicensePlate is the owner-entered plate (MYR-286). Prisma-owned
	// column, NOT NULL DEFAULT '' — empty string means "no plate set",
	// never "unknown". Server-normalized on write (see
	// VehicleRepo.UpdateLicensePlate), so reads are already uppercase,
	// trimmed, and within the ≤10-char A-Z/0-9/space/hyphen charset.
	LicensePlate         string
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

	// MYR-269 / MYR-273 owner-control read-backs, hydrated from the Go-owned
	// go_vehicle_control_state side table via a LEFT JOIN on the GetByID
	// (snapshot) read path. All nullable — a nil pointer means the field was
	// never read for this vehicle. Populated only by GetByID; GetByVIN and
	// ListByUser leave them nil (they do not join the side table).
	IsLocked       *bool
	FrunkOpen      *bool
	TrunkOpen      *bool
	IsClimateOn    *bool
	ChargePortOpen *bool

	// MYR-273 cabin-setting levels (temp setpoints, fan speed, seat heater/
	// cooler levels, media volume). Same side table, same nil-means-never-read
	// semantics.
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

	// MYR-279 vehicle-detail read-backs (software version, trim), hydrated from
	// the same go_vehicle_control_state side table on the GetByID snapshot read.
	// Nullable — nil means never read. Populated only by GetByID.
	SoftwareVersion *string
	Trim            *string

	// MYR-274 climate-mode read-backs (hvac auto mode string, A/C enabled bool),
	// same side table, same nil-means-never-read semantics. Populated only by
	// GetByID.
	HvacAutoMode  *string
	HvacAcEnabled *bool

	// MYR-298 seat-ventilation + media-playback read-backs, same side table,
	// same nil-means-never-read semantics. Populated only by GetByID.
	SeatVentEnabled     *bool
	MediaPlaybackStatus *string

	// MYR-303 media now-playing block, same side table, populated only by
	// GetByID. nil means NEVER OBSERVED. For the five text fields that is
	// distinct from a non-nil EMPTY string, which means "observed, and nothing
	// is playing" — see mapControlMediaNowPlaying (control_state_media.go).
	MediaNowPlayingTitle    *string
	MediaNowPlayingArtist   *string
	MediaNowPlayingAlbum    *string
	MediaNowPlayingStation  *string
	MediaPlaybackSource     *string
	MediaNowPlayingDuration *int64
	MediaNowPlayingElapsed  *int64
	MediaVolumeMax          *float64

	// MYR-308 ventilated-seat capability (a spec fact from REST vehicle_config,
	// not a runtime state), same side table, populated only by GetByID. nil
	// means never read — NOT "no seat cooling"; an explicit false is the
	// authoritative no.
	SeatCoolingCapable *bool

	// MYR-316 service window: the two independent sources behind the single
	// wire field serviceEstimatedEndAt, same side table. ServiceETC is Tesla's
	// own estimate and takes precedence; ServiceExpectedEndAt is the
	// owner-entered fallback. Emission is COALESCE(ServiceETC,
	// ServiceExpectedEndAt) and ONLY while Status is in_service — the read
	// layer applies both rules, so these two are raw storage, not the wire
	// value.
	ServiceETC           *time.Time
	ServiceExpectedEndAt *time.Time

	// MYR-320 vehicle-detail read-backs, same side table, populated only by
	// GetByID. TrimLabel is the DISPLAY-SAFE trim label
	// (vehicle_config.performance_package) and stands ALONGSIDE Trim above,
	// which keeps the raw badge code; FSDVersion is the FSD software
	// designation from the newest release-notes title and is a different value
	// from SoftwareVersion above, which is the firmware build. nil means never
	// read — for FSDVersion emphatically NOT "this car has no FSD".
	TrimLabel  *string
	FSDVersion *string

	// RideShareEnabled is the owner's ride-sharing switch (MYR-342), same side
	// table. A plain bool, NOT a pointer, and the only field in this block that
	// is not: migration 0021 declares the column NOT NULL DEFAULT true and the
	// read wraps it in COALESCE(..., TRUE) (rideShareEnabledExpr), so "no
	// side-table row" and "row with the column defaulted" both arrive here as
	// true. There is no unknown state to represent — a car nobody has paused is
	// accepting rides — and a pointer would have invented one that every reader
	// collapsed back to true anyway.
	//
	// POPULATED ONLY BY GetByID, and unlike its nil-able neighbours the zero
	// value POINTS THE WRONG WAY: a Vehicle from GetByVIN or the wide ListByUser
	// (neither joins the side table) carries false, which reads as PAUSED. Do
	// not surface or enforce this field off those paths. Nothing does today —
	// the two request-time gates read it off the very GetByID row that
	// established ownership (one statement, no TOCTOU between the two facts),
	// and the reservation sweeper uses the dedicated VehicleRepo.RideShareEnabled
	// statement instead of a vehicle read at all.
	RideShareEnabled bool
}

// VehicleUpdate holds the subset of vehicle fields that can change from
// a single telemetry event. Nil pointer fields are not written to the
// database, allowing partial updates.
//
// MYR-63 dual-write rollout: the six GPS columns are persisted as a
// plaintext Float ALONGSIDE a *Enc TEXT shadow. Set the plaintext
// pointers from the telemetry pipeline as before; the encrypted
// shadows are computed inside VehicleRepo.UpdateTelemetry from the
// same values. Half-pair input (lat without lng or vice versa) skips
// the *Enc write entirely — see vehicle_gps_encryption.go.
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

	// NavReadingAt is set ONLY when this frame carried a navigation-group member,
	// zero otherwise (MYR-409) — the distinction LastUpdated cannot make, since
	// that one moves on every write of any column. Decided by carriedNavReading
	// (field_mapper.go), persisted to the Go-owned side table by
	// VehicleRepo.SetNavReadingAt (vehicle_nav_freshness.go, migration 0034) and
	// never to the Prisma-owned car row. Monotone on write and on merge.
	NavReadingAt time.Time

	// Streamed reports whether this update came from the LIVE vehicle stream
	// rather than a REST backfill (MYR-260's synthetic /vehicle_data frames).
	// Provenance has to survive the trip into the store because MYR-454's
	// status fold must not act on a REST frame: those are published ONLY for
	// cars the monitor has just determined are NOT streaming (in service,
	// asleep, or offline), they carry Tesla's CACHED speed and shift_state,
	// and the whole justification for folding `offline` down to `parked` is
	// that a frame arriving proves the car is live. For a backfill frame it
	// proves the opposite. Coalescing ORs this across the batch — one live
	// frame in the window is enough to make the merged update stream-backed.
	Streamed bool

	// ControlState carries the MYR-269 owner-control read-backs (lock, trunk/
	// frunk, climate, charge-port) destined for the Go-owned
	// go_vehicle_control_state SIDE table — NOT the Prisma-owned "Vehicle"
	// table. buildTelemetryUpdate ignores it; the writer flush upserts it
	// separately (writer_flush.go). Nil when a frame carries no control fields.
	ControlState *ControlStateUpdate
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
//
// StartChargeLevel is (re)written here as well as at insert time because the
// Drive row is created on drive.started — a DriveStartedEvent that carries no
// charge — so the insert always defaults startChargeLevel to 0. The drive
// detector only knows the true drive-start SOC by the time the drive ends
// (it seeds from the last-known charge, or lazily from the first in-drive SOC
// sample; see internal/drives). Persisting evt.Stats.StartChargeLevel on the
// completion UPDATE is what actually lands that value in the DB (MYR-241).
type DriveCompletion struct {
	EndTime          string
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
