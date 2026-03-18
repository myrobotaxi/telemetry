package drives

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

// debounceCallback is invoked by time.AfterFunc when the debounce period
// elapses. It runs on a timer goroutine and must acquire state.mu.
func (d *Detector) debounceCallback(vin string) {
	// Check if the detector has been stopped.
	select {
	case <-d.ctx.Done():
		return
	default:
	}

	val, ok := d.states.Load(vin)
	if !ok {
		return
	}
	state := val.(*vehicleState)

	state.mu.Lock()
	defer state.mu.Unlock()

	// Guard: state may have changed between timer firing and lock acquisition.
	if state.status != StatusDriving {
		return
	}
	if state.debounceTimer == nil {
		return // timer was cancelled between firing and lock acquisition
	}

	d.endDrive(state, vin)
}

// endDrive completes a drive, calculates stats, applies micro-drive
// filtering, and publishes DriveEndedEvent. The caller must hold state.mu.
func (d *Detector) endDrive(state *vehicleState, vin string) {
	drive := state.drive
	if drive == nil {
		resetToIdle(state)
		return
	}

	stats := calculateStats(drive)

	// Micro-drive filtering: discard short or short-distance drives.
	if stats.Duration < d.cfg.MinDuration || stats.Distance < d.cfg.MinDistanceMiles {
		d.logger.Info("discarding micro-drive",
			slog.String("vin", redactVIN(vin)),
			slog.String("drive_id", drive.id),
			slog.Duration("duration", stats.Duration),
			slog.Float64("distance_miles", stats.Distance),
		)
		d.metrics.IncMicroDriveDiscarded()
		resetToIdle(state)
		d.activeCount.Add(-1)
		d.metrics.SetActiveVehicles(int(d.activeCount.Load()))
		return
	}

	d.metrics.IncDriveEnded()
	d.metrics.ObserveDriveDuration(stats.Duration.Seconds())
	d.metrics.ObserveDriveDistance(stats.Distance)

	d.logger.Info("drive ended",
		slog.String("vin", redactVIN(vin)),
		slog.String("drive_id", drive.id),
		slog.Duration("duration", stats.Duration),
		slog.Float64("distance_miles", stats.Distance),
	)

	driveID := drive.id
	endedAt := drive.lastTimestamp
	resetToIdle(state)
	d.activeCount.Add(-1)
	d.metrics.SetActiveVehicles(int(d.activeCount.Load()))

	// Publish DriveEndedEvent.
	evt := events.NewEvent(events.DriveEndedEvent{
		VIN:     vin,
		DriveID: driveID,
		Stats:   stats,
		EndedAt: endedAt,
	})
	if err := d.bus.Publish(d.ctx, evt); err != nil {
		d.logger.Error("failed to publish DriveEndedEvent",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
	}
}

// publishDriveUpdated publishes a DriveUpdatedEvent for a single route point.
func (d *Detector) publishDriveUpdated(vin, driveID string, rp events.RoutePoint) {
	evt := events.NewEvent(events.DriveUpdatedEvent{
		VIN:        vin,
		DriveID:    driveID,
		RoutePoint: rp,
	})
	if err := d.bus.Publish(d.ctx, evt); err != nil {
		d.logger.Error("failed to publish DriveUpdatedEvent",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
	}
}

// generateDriveID produces a random hex identifier for a new drive.
func generateDriveID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
