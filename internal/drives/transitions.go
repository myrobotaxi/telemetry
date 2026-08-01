package drives

import (
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// isDriveGear returns true if the gear value indicates the vehicle is moving.
func isDriveGear(gear string) bool {
	return gear == "D" || gear == "R"
}

// restDisplacementMiles is how far a NON-STREAMED position fix must sit from
// the previous one before it counts as movement. It is the STREAM's own
// Location filter — `MinimumDelta: 10` metres in DefaultFieldConfig
// (internal/telemetry/fleet_api_fields.go) — restated in the miles that
// haversine returns.
//
// WHY THIS THRESHOLD EXISTS AT ALL (MYR-394). Before REST position polling, the
// only source of `location` was the stream, and Tesla applied that 10m delta
// filter on the CAR: a parked, streaming vehicle sent gear and speed but not a
// repeated Location, so handleDriving's location branch never ran and the
// MYR-160 stall watchdog could see a motionless drive and close it. A REST
// vehicle_data read has no such filter — it returns the car's cached fix on
// every poll, byte-identical for a car that has not moved. Taking each of those
// as "movement" would refresh lastMovementAt every ~25s forever, so StallTimeout
// could never fire, and an open drive on a parked car would accumulate an
// identical route point (and broadcast a DriveUpdatedEvent) every cycle until
// MaxDriveDuration's 12h backstop. Matching the stream's own delta means the
// two sources agree on what "moved" means.
const restDisplacementMiles = 0.0062137 // 10 metres

// displacedEnough reports whether te's location is far enough from the drive's
// previous fix to count as real motion.
//
// A STREAMED frame is taken at face value: Tesla already applied the 10m
// MinimumDelta on the car, so its arrival IS the movement signal, and
// re-deriving the same judgement from sparse GPS here would risk discarding
// genuine points at the boundary. Only the REST-backfilled frames — which carry
// no delta filter of their own — are measured.
func displacedEnough(te events.VehicleTelemetryEvent, prev, next events.Location) bool {
	if te.Streamed() {
		return true
	}
	return haversine(prev.Latitude, prev.Longitude, next.Latitude, next.Longitude) >= restDisplacementMiles
}

// handleIdle processes a telemetry event when the vehicle is in StatusIdle.
// It transitions to StatusDriving when the gear shifts to D or R.
// The caller must hold state.mu.
func (d *Detector) handleIdle(state *vehicleState, vin string, te events.VehicleTelemetryEvent) {
	if !isDriveGear(state.lastGear) {
		return
	}

	d.startDrive(state, vin, te)
}

// handleDriving processes a telemetry event when the vehicle is in
// StatusDriving. It accumulates route points, updates running stats,
// manages debounce on gear P, and cancels debounce on gear D/R.
// The caller must hold state.mu.
func (d *Detector) handleDriving(state *vehicleState, vin string, te events.VehicleTelemetryEvent) {
	drive := state.drive
	if drive == nil {
		return
	}

	// moved flags real motion in this event (positive speed, new route
	// point, or odometer advance). Feeds the watchdog's stall condition
	// (MYR-160): a drive whose telemetry keeps flowing without movement
	// for StallTimeout is a parked car whose gear=P frame was missed.
	moved := accumulateDriveCounters(drive, te)

	// Accumulate route point if location is present AND the car has actually
	// moved since the last fix.
	if loc := extractLocation(te.Fields); loc != nil {
		// The drive's clock advances on EVERY located frame, moved or not:
		// calculateStats derives duration from lastTimestamp, and a stationary
		// car still has an honest end time.
		drive.lastTimestamp = te.CreatedAt

		if displacedEnough(te, drive.lastLocation, *loc) {
			heading, _ := extractFloatField(te.Fields, telemetry.FieldHeading)
			speed, _ := extractFloatField(te.Fields, telemetry.FieldSpeed)

			rp := events.RoutePoint{
				Latitude:  loc.Latitude,
				Longitude: loc.Longitude,
				Speed:     speed,
				Heading:   heading,
				Timestamp: te.CreatedAt,
			}
			drive.routePoints = append(drive.routePoints, rp)
			drive.lastLocation = *loc
			moved = true

			// Publish DriveUpdatedEvent for each location-bearing tick.
			d.publishDriveUpdated(vin, drive.id, rp)
		}
	}

	if moved {
		drive.lastMovementAt = d.now()
	}

	// Check for gear transitions that affect drive state.
	gear := state.lastGear
	switch {
	case gear == "P":
		d.startDebounce(state, vin)
	case isDriveGear(gear) && state.debounceTimer != nil:
		d.cancelDebounce(state, vin)
	}
}

// accumulateDriveCounters folds the event's speed/FSD/odometer/SOC/
// energy samples into the active drive and reports whether the event
// carried a movement signal (positive speed or odometer advance).
// FSD and odometer baselines are lazily seeded from the first in-drive
// sample when they were cold at drive start (MYR-157). The caller must
// hold state.mu.
func accumulateDriveCounters(drive *activeDrive, te events.VehicleTelemetryEvent) bool {
	moved := false

	if speed, ok := extractFloatField(te.Fields, telemetry.FieldSpeed); ok {
		drive.speedSum += speed
		drive.speedCount++
		if speed > drive.maxSpeed {
			drive.maxSpeed = speed
		}
		if speed > 0 {
			moved = true
		}
	}

	if fsd, ok := extractFloatField(te.Fields, telemetry.FieldFSDMiles); ok {
		if !drive.fsdBaselineSet {
			drive.startFSDMiles = fsd
			drive.fsdBaselineSet = true
		}
		drive.lastFSDMiles = fsd
	}

	if odo, ok := extractFloatField(te.Fields, telemetry.FieldOdometer); ok {
		if !drive.odometerBaselineSet {
			drive.startOdometer = odo
			drive.odometerBaselineSet = true
		} else if odo > drive.lastOdometer {
			moved = true
		}
		drive.lastOdometer = odo
	}

	if soc, ok := extractFloatField(te.Fields, telemetry.FieldSOC); ok {
		// Lazily seed the start charge from the first in-drive SOC sample
		// when it was cold at drive start (no cached idle value) — better
		// than persisting 0. Mirrors the FSD/odometer baseline (MYR-157,
		// MYR-207).
		if !drive.startChargeSet && soc > 0 {
			drive.startCharge = soc
			drive.startChargeSet = true
		}
		drive.lastSOC = soc
	}

	if energy, ok := extractFloatField(te.Fields, telemetry.FieldEnergyRemaining); ok {
		drive.lastEnergy = energy
	}

	return moved
}

// startDrive transitions the vehicle from Idle to Driving.
// The caller must hold state.mu.
func (d *Detector) startDrive(state *vehicleState, vin string, te events.VehicleTelemetryEvent) {
	driveID := generateDriveID()

	// Determine start location: prefer current event, fall back to cached.
	// Treat (0,0) as "not set" — protobuf default for unset GPS coordinates.
	var startLoc events.Location
	if loc := extractLocation(te.Fields); loc != nil && (loc.Latitude != 0 || loc.Longitude != 0) {
		startLoc = *loc
	} else if state.lastLocation != nil && (state.lastLocation.Latitude != 0 || state.lastLocation.Longitude != 0) {
		startLoc = *state.lastLocation
	}

	// Seed the start charge (SOC %) from the last-known value cached for
	// this vehicle (including while idle), preferring a fresh non-zero SOC
	// on the triggering frame. The gear-change frame that starts a drive
	// often lacks the charge atomic group; without this seed
	// startChargeLevel persisted as 0 while endChargeLevel captured
	// correctly, yielding a nonsense "0% -> 75%" summary (MYR-207). SOC
	// does not change while parked, so the last idle sample is the correct
	// drive-start charge. If neither source has a value, the baseline stays
	// unset and accumulateDriveCounters lazily captures it from the first
	// in-drive SOC sample (mirrors the FSD/odometer baseline, MYR-157).
	startCharge := state.lastSOC
	startChargeSet := state.socKnown
	if soc, ok := extractFloatField(te.Fields, telemetry.FieldSOC); ok && soc > 0 {
		startCharge = soc
		startChargeSet = true
	}

	var startEnergy float64
	if energy, ok := extractFloatField(te.Fields, telemetry.FieldEnergyRemaining); ok {
		startEnergy = energy
	}

	// Seed the FSD + odometer baselines from the most recent values seen for
	// this vehicle (cached in handleTelemetry, including while idle). Both
	// are cumulative counters that don't advance while parked, so the last
	// value before the drive is the correct baseline; the gear-change frame
	// that starts the drive usually lacks them (slow cadence). If none has
	// been observed, the baseline stays unset and handleDriving lazily
	// captures it from the first in-drive sample (MYR-157).
	now := d.now()
	drive := &activeDrive{
		id:                  driveID,
		startedAt:           te.CreatedAt,
		startedWall:         now,
		lastMovementAt:      now,
		startLocation:       startLoc,
		maxSpeed:            0,
		startCharge:         startCharge,
		startChargeSet:      startChargeSet,
		startOdometer:       state.lastOdometer,
		lastOdometer:        state.lastOdometer,
		odometerBaselineSet: state.odometerKnown,
		startEnergy:         startEnergy,
		startFSDMiles:       state.lastFSDMiles,
		lastFSDMiles:        state.lastFSDMiles,
		fsdBaselineSet:      state.fsdMilesKnown,
		lastLocation:        startLoc,
		lastTimestamp:       te.CreatedAt,
		lastSOC:             startCharge,
		lastEnergy:          startEnergy,
	}

	state.status = StatusDriving
	state.drive = drive

	d.metrics.IncDriveStarted()
	d.activeCount.Add(1)
	d.metrics.SetActiveVehicles(int(d.activeCount.Load()))

	d.logger.Info("drive started",
		slog.String("vin", redactVIN(vin)),
		slog.String("drive_id", driveID),
	)

	// Publish DriveStartedEvent.
	evt := events.NewEvent(events.DriveStartedEvent{
		VIN:       vin,
		DriveID:   driveID,
		Location:  startLoc,
		StartedAt: te.CreatedAt,
	})
	if err := d.bus.Publish(d.ctx, evt); err != nil {
		d.logger.Error("failed to publish DriveStartedEvent",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
	}
}

// startDebounce starts the debounce timer for drive ending. If a timer is
// already running, this is a no-op. The caller must hold state.mu.
func (d *Detector) startDebounce(state *vehicleState, vin string) {
	if state.debounceTimer != nil {
		return // already debouncing
	}

	state.debounceTimer = time.AfterFunc(d.cfg.EndDebounce, func() {
		d.debounceCallback(vin)
	})

	d.logger.Debug("drive debounce started",
		slog.String("vin", redactVIN(vin)),
		slog.Duration("debounce", d.cfg.EndDebounce),
	)
}

// cancelDebounce stops the debounce timer, allowing the drive to continue.
// The caller must hold state.mu.
func (d *Detector) cancelDebounce(state *vehicleState, vin string) {
	if state.debounceTimer == nil {
		return
	}

	state.debounceTimer.Stop()
	state.debounceTimer = nil

	d.metrics.IncDebounceCancelled()
	d.logger.Debug("drive debounce cancelled, drive continues",
		slog.String("vin", redactVIN(vin)),
	)
}
