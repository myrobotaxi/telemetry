package drives

import (
	"log/slog"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

// isDriveGear returns true if the gear value indicates the vehicle is moving.
func isDriveGear(gear string) bool {
	return gear == "D" || gear == "R"
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

	// Accumulate speed stats.
	if speed, ok := extractFloatField(te.Fields, telemetry.FieldSpeed); ok {
		drive.speedSum += speed
		drive.speedCount++
		if speed > drive.maxSpeed {
			drive.maxSpeed = speed
		}
	}

	// Update FSD miles tracking.
	if fsd, ok := extractFloatField(te.Fields, telemetry.FieldFSDMiles); ok {
		drive.lastFSDMiles = fsd
	}

	// Update SOC tracking.
	if soc, ok := extractFloatField(te.Fields, telemetry.FieldSOC); ok {
		drive.lastSOC = soc
	}

	// Update energy tracking.
	if energy, ok := extractFloatField(te.Fields, telemetry.FieldEnergyRemaining); ok {
		drive.lastEnergy = energy
	}

	// Accumulate route point if location is present.
	if loc := extractLocation(te.Fields); loc != nil {
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
		drive.lastTimestamp = te.CreatedAt

		// Publish DriveUpdatedEvent for each location-bearing tick.
		d.publishDriveUpdated(vin, drive.id, rp)
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

// startDrive transitions the vehicle from Idle to Driving.
// The caller must hold state.mu.
func (d *Detector) startDrive(state *vehicleState, vin string, te events.VehicleTelemetryEvent) {
	driveID := generateDriveID()

	// Determine start location: prefer current event, fall back to cached.
	var startLoc events.Location
	if loc := extractLocation(te.Fields); loc != nil {
		startLoc = *loc
	} else if state.lastLocation != nil {
		startLoc = *state.lastLocation
	}

	// Snapshot initial values from the event.
	var startCharge float64
	if soc, ok := extractFloatField(te.Fields, telemetry.FieldSOC); ok {
		startCharge = soc
	}

	var startOdometer float64
	if odo, ok := extractFloatField(te.Fields, telemetry.FieldOdometer); ok {
		startOdometer = odo
	}

	var startEnergy float64
	if energy, ok := extractFloatField(te.Fields, telemetry.FieldEnergyRemaining); ok {
		startEnergy = energy
	}

	var startFSDMiles float64
	if fsd, ok := extractFloatField(te.Fields, telemetry.FieldFSDMiles); ok {
		startFSDMiles = fsd
	}

	drive := &activeDrive{
		id:            driveID,
		startedAt:     te.CreatedAt,
		startLocation: startLoc,
		maxSpeed:      0,
		startCharge:   startCharge,
		startOdometer: startOdometer,
		startEnergy:   startEnergy,
		startFSDMiles: startFSDMiles,
		lastFSDMiles:  startFSDMiles,
		lastLocation:  startLoc,
		lastTimestamp:  te.CreatedAt,
		lastSOC:       startCharge,
		lastEnergy:    startEnergy,
	}

	state.status = StatusDriving
	state.drive = drive

	d.metrics.IncDriveStarted()
	d.updateActiveVehicleGauge()

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

