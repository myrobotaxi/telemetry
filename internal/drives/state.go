package drives

import (
	"sync"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
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

	// lastFSDMiles caches the most recent fsdMilesSinceReset value seen for
	// this vehicle, including while idle. fsdMilesKnown reports whether a
	// value has ever been observed. FSD miles is a cumulative "miles since
	// reset" counter streamed on a slow cadence (60s / 1-mile delta) while
	// gear streams every 1s, so the gear-change event that starts a drive
	// almost never carries FSD miles. Caching it here lets startDrive seed
	// the drive baseline from the last value seen before the drive began —
	// the correct baseline, since FSD miles do not accumulate while parked.
	lastFSDMiles  float64
	fsdMilesKnown bool

	// lastTelemetryAt records the wall-clock time of the most recently
	// received telemetry event for this vehicle (any field, gear-bearing
	// or not). The end-condition watchdog uses this to detect drives
	// that have gone silent because Tesla stopped streaming when the car
	// parked. See watchdogTick in debounce.go.
	lastTelemetryAt time.Time
}

// activeDrive accumulates data during an in-progress drive.
type activeDrive struct {
	id             string
	startedAt      time.Time
	startLocation  events.Location
	routePoints    []events.RoutePoint
	maxSpeed       float64
	speedSum       float64 // running sum for average calculation
	speedCount     int     // number of speed samples
	startCharge    float64 // SOC at drive start (percent)
	startOdometer  float64 // odometer at drive start (miles)
	startEnergy    float64 // energyRemaining at drive start (kWh)
	startFSDMiles  float64 // fsdMilesSinceReset baseline for this drive
	lastFSDMiles   float64 // most recent fsdMilesSinceReset seen
	fsdBaselineSet bool    // true once startFSDMiles holds a real observed value
	lastLocation   events.Location
	lastTimestamp  time.Time
	lastSOC        float64 // most recent SOC for EndChargeLevel
	lastEnergy     float64 // most recent energyRemaining for EnergyDelta

	// reconciled is true when this drive was reattached from a DB row
	// orphaned by a previous restart (MYR-146). endDrive bypasses the
	// micro-drive filter for reconciled drives: with no resumed
	// telemetry, calculateStats returns Duration=0, Distance=0, which
	// the prod-default filter (MinDuration=2m, MinDistanceMiles=0.1)
	// would otherwise discard -- leaving the open DB row forever and
	// reconciling it again on the next restart. Closing those rows is
	// the whole point of reconciliation, so we publish DriveEndedEvent
	// unconditionally for them. Cleared once real telemetry arrives
	// (see handleDriving), so a reconciled drive that survives the
	// restart and accumulates legitimate route data is filtered
	// normally on its real end transition.
	reconciled bool
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
