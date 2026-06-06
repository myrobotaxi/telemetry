package drives

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// runWatchdog periodically scans all per-vehicle states and ends any
// drive whose last telemetry frame is older than EndDebounce. Tesla
// stops streaming when the vehicle parks, so the gear=P -> AfterFunc
// path is not guaranteed to fire; this watchdog is the safety net that
// guarantees DriveEndedEvent emission even when streaming goes silent.
//
// MYR-139 R3a: without this, stuck Drive rows accumulate in the DB with
// endTime IS NULL because the writer subscribes to drive.ended and the
// detector never publishes it.
func (d *Detector) runWatchdog() {
	defer d.watchdogWG.Done()

	ticker := time.NewTicker(d.watchdogInterval())
	defer ticker.Stop()

	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.watchdogTick()
		}
	}
}

// watchdogTick walks every active per-vehicle state and ends drives
// whose last telemetry arrival is older than EndDebounce.
//
// Locking: each vehicle lock is acquired in its own iteration so a slow
// endDrive (which publishes to the bus) does not block other vehicles.
// We tolerate sync.Map's "no snapshot" semantics because Range visits
// each key at most once even under concurrent insertion.
func (d *Detector) watchdogTick() {
	now := d.now()
	cutoff := d.cfg.EndDebounce

	d.states.Range(func(key, value any) bool {
		vin, _ := key.(string)
		state, _ := value.(*vehicleState)
		if state == nil {
			return true
		}

		state.mu.Lock()
		// Guard 1: only active drives are candidates.
		if state.status != StatusDriving {
			state.mu.Unlock()
			return true
		}
		// Guard 2: never preempt an in-flight gear=P debounce timer.
		// Letting the AfterFunc fire keeps debounce semantics intact
		// (cancellable up to EndDebounce after gear=P) and avoids a
		// double-end race.
		if state.debounceTimer != nil {
			state.mu.Unlock()
			return true
		}
		// Guard 3: a drive in flight must have had at least one
		// telemetry frame already (the start frame). If not, leave
		// it alone -- the watchdog only acts on observed silence.
		if state.lastTelemetryAt.IsZero() {
			state.mu.Unlock()
			return true
		}
		// The actual condition: telemetry has been silent for at
		// least EndDebounce.
		if now.Sub(state.lastTelemetryAt) < cutoff {
			state.mu.Unlock()
			return true
		}

		d.logger.Info("ending drive via watchdog (telemetry silent)",
			slog.String("vin", redactVIN(vin)),
			slog.Duration("silent_for", now.Sub(state.lastTelemetryAt)),
			slog.Duration("end_debounce", cutoff),
		)
		d.metrics.IncWatchdogEnded()
		d.endDrive(state, vin)
		state.mu.Unlock()
		return true
	})
}

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
