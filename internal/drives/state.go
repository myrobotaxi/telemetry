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

	// lastOdometer caches the most recent odometer value seen for this
	// vehicle, including while idle (mirrors lastFSDMiles). odometerKnown
	// reports whether a value has been observed. Odometer streams on a slow
	// cadence (60s), so the gear-change event that starts a drive usually
	// lacks it; caching lets startDrive seed an accurate baseline (the car
	// has not moved since the last parked sample). calculateStats derives
	// drive distance from the odometer delta (MYR-157), which is far more
	// accurate than the GPS-haversine of sparse route points.
	lastOdometer  float64
	odometerKnown bool

	// lastSOC caches the most recent state-of-charge value seen for this
	// vehicle, including while idle (mirrors lastFSDMiles/lastOdometer).
	// socKnown reports whether a plausible (non-zero) value has been
	// observed. The charge atomic group streams on a slower cadence than
	// gear, so the gear-change frame that starts a drive frequently lacks
	// SOC; without a cache startDrive recorded startChargeLevel=0 while
	// endChargeLevel captured correctly, producing a nonsense "0% -> 75%,
	// -75% used" summary (MYR-207). SOC does not change while parked, so
	// the last idle sample is the correct drive-start charge. Only
	// non-zero values are cached: a literal 0 is the protobuf/zero-value
	// default we are guarding against, never a plausible parked charge.
	lastSOC  float64
	socKnown bool

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
	startChargeSet bool    // true once startCharge holds a real observed value (MYR-207)
	startOdometer  float64 // odometer at drive start (miles)
	startEnergy    float64 // energyRemaining at drive start (kWh)
	startFSDMiles  float64 // fsdMilesSinceReset baseline for this drive
	lastFSDMiles   float64 // most recent fsdMilesSinceReset seen
	fsdBaselineSet bool    // true once startFSDMiles holds a real observed value
	// Odometer distance (MYR-157): distance = lastOdometer - startOdometer
	// when odometerBaselineSet, else fall back to GPS totalDistance.
	lastOdometer        float64 // most recent odometer reading (miles)
	odometerBaselineSet bool    // true once startOdometer holds a real observed value
	lastLocation        events.Location
	lastTimestamp       time.Time
	lastSOC             float64 // most recent SOC for EndChargeLevel
	lastEnergy          float64 // most recent energyRemaining for EnergyDelta

	// startedWall and lastMovementAt are wall-clock (Detector.now)
	// timestamps backing the watchdog's stall and duration-cap end
	// conditions (MYR-160). startedAt/lastTimestamp carry vehicle
	// event times, which cannot safely be compared against the
	// watchdog's clock. lastMovementAt advances whenever the drive
	// shows real motion: a positive speed sample, a new route point,
	// or an odometer increase.
	startedWall    time.Time
	lastMovementAt time.Time
}

// resetToIdle resets the vehicle state to idle. The caller must hold s.mu.
//
// lastGear is CLEARED, and that is not housekeeping — it is the guard against
// phantom drive starts (MYR-394). handleIdle's only entry condition is
// `isDriveGear(state.lastGear)`, so whatever gear is left here decides whether
// the NEXT frame of any kind opens a new drive. Every non-gear-driven end —
// watchdog silence, stall, the duration cap, a connectivity drop — leaves
// lastGear at "D" precisely because no gear=P frame was ever seen; that is why
// those end conditions exist. Leaving it set means the first subsequent frame
// that carries any field at all re-enters handleIdle and mints a fresh drive
// for a car that is parked. That was harmless while only the stream produced
// frames (the next stream frame carried a real gear, correcting the cache), but
// REST backfill frames arrive every ~25s during a ride and Tesla answers
// shift_state=null for a parked car, so nothing would ever correct it.
//
// Clearing to "" is the honest value: we do not know the gear until a frame
// tells us. isDriveGear("") is false, so no drive can start on a guess.
func resetToIdle(s *vehicleState) {
	if s.debounceTimer != nil {
		s.debounceTimer.Stop()
		s.debounceTimer = nil
	}
	s.status = StatusIdle
	s.drive = nil
	s.lastGear = ""
}
