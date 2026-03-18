package drives

import (
	"sync"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

// DriveStatus represents the current state of a vehicle's drive detection.
type DriveStatus int

const (
	// StatusIdle means the vehicle is not driving (gear is P, N, or unknown).
	StatusIdle DriveStatus = iota
	// StatusDriving means the vehicle is actively driving (gear is D or R).
	StatusDriving
)

// String returns a human-readable drive status label.
func (s DriveStatus) String() string {
	switch s {
	case StatusIdle:
		return "idle"
	case StatusDriving:
		return "driving"
	default:
		return "unknown"
	}
}

// vehicleState tracks the drive-detection state for a single vehicle.
// Each vehicle gets its own instance stored in the Detector's sync.Map.
//
// The per-vehicle mutex serializes access between the bus handler goroutine
// (which processes telemetry events) and debounce timer callbacks (which fire
// on a separate goroutine managed by time.AfterFunc).
type vehicleState struct {
	mu sync.Mutex // guards all fields below

	status DriveStatus
	drive  *activeDrive // non-nil only when status == StatusDriving

	// debounceTimer is set when gear transitions to P during a drive.
	// If gear returns to D/R before the timer fires, the timer is cancelled
	// and the drive continues. If the timer fires, the drive ends.
	debounceTimer *time.Timer

	// lastGear caches the most recent gear value to detect transitions.
	lastGear string

	// lastLocation caches the most recent valid location for drives that
	// start without a location in the triggering event.
	lastLocation *events.Location
}

// activeDrive accumulates data during an in-progress drive.
type activeDrive struct {
	id            string
	startedAt     time.Time
	startLocation events.Location
	routePoints   []events.RoutePoint
	maxSpeed      float64
	speedSum      float64 // running sum for average calculation
	speedCount    int     // number of speed samples
	startCharge   float64 // SOC at drive start (percent)
	startOdometer float64 // odometer at drive start (miles)
	startEnergy   float64 // energyRemaining at drive start (kWh)
	startFSDMiles float64 // fsdMilesSinceReset at drive start
	lastFSDMiles  float64 // most recent fsdMilesSinceReset seen
	lastLocation  events.Location
	lastTimestamp time.Time
	lastSOC       float64 // most recent SOC for EndChargeLevel
	lastEnergy    float64 // most recent energyRemaining for EnergyDelta
}

// resetToIdle resets the vehicle state to idle. The caller must hold s.mu.
func resetToIdle(s *vehicleState) {
	if s.debounceTimer != nil {
		s.debounceTimer.Stop()
		s.debounceTimer = nil
	}
	s.status = StatusIdle
	s.drive = nil
}
