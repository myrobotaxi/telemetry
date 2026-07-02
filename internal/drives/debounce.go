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
// that meet one of the watchdog end conditions (see watchdogEndReason).
//
// Locking: each vehicle lock is acquired in its own iteration so a slow
// endDrive (which publishes to the bus) does not block other vehicles.
// We tolerate sync.Map's "no snapshot" semantics because Range visits
// each key at most once even under concurrent insertion.
func (d *Detector) watchdogTick() {
	now := d.now()

	d.states.Range(func(key, value any) bool {
		vin, _ := key.(string)
		state, _ := value.(*vehicleState)
		if state == nil {
			return true
		}

		state.mu.Lock()
		// Guard 1: only active drives are candidates.
		if state.status != StatusDriving || state.drive == nil {
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

		reason := d.watchdogEndReason(state, now)
		if reason == "" {
			state.mu.Unlock()
			return true
		}

		d.logger.Info("ending drive via watchdog",
			slog.String("vin", redactVIN(vin)),
			slog.String("drive_id", state.drive.id),
			slog.String("reason", reason),
		)
		d.endDrive(state, vin)
		state.mu.Unlock()
		return true
	})
}

// watchdogEndReason evaluates the three watchdog end conditions for an
// active drive and returns a human-readable reason, or "" when the
// drive should stay open. Increments the per-cause metric as a side
// effect so callers can't end a drive without accounting for it. The
// caller must hold state.mu and guarantee state.drive != nil.
func (d *Detector) watchdogEndReason(state *vehicleState, now time.Time) string {
	// Telemetry silence: Tesla stops streaming when the vehicle parks,
	// so a gear=P close-frame is not guaranteed (MYR-139 R3a). The
	// drive must have seen at least one frame (the start frame) — the
	// watchdog only acts on observed silence.
	if !state.lastTelemetryAt.IsZero() && now.Sub(state.lastTelemetryAt) >= d.cfg.EndDebounce {
		d.metrics.IncWatchdogEnded()
		return "telemetry silent"
	}

	drive := state.drive

	// Stall (MYR-160): telemetry keeps flowing but nothing indicates
	// motion. This is the missed-Park-frame case — the car is parked
	// and streaming idle fields, so neither the gear debounce nor the
	// silence condition above can ever fire.
	if d.cfg.StallTimeout > 0 && !drive.lastMovementAt.IsZero() &&
		now.Sub(drive.lastMovementAt) >= d.cfg.StallTimeout {
		d.metrics.IncStallEnded()
		return "no movement while telemetry streaming (stall)"
	}

	// Duration cap (MYR-160): hard backstop on active-drive age so no
	// combination of missed frames and spurious movement signals can
	// leave a drive open indefinitely.
	if d.cfg.MaxDriveDuration > 0 && !drive.startedWall.IsZero() &&
		now.Sub(drive.startedWall) >= d.cfg.MaxDriveDuration {
		d.metrics.IncDurationCapEnded()
		return "max active-drive duration exceeded"
	}

	return ""
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
	// The writer created a Drive row on drive.started, so a silent
	// discard would leak an open row with endTime unset (MYR-160);
	// DriveDiscardedEvent tells the store to delete it.
	if stats.Duration < d.cfg.MinDuration || stats.Distance < d.cfg.MinDistanceMiles {
		d.logger.Info("discarding micro-drive",
			slog.String("vin", redactVIN(vin)),
			slog.String("drive_id", drive.id),
			slog.Duration("duration", stats.Duration),
			slog.Float64("distance_miles", stats.Distance),
		)
		d.metrics.IncMicroDriveDiscarded()
		driveID := drive.id
		resetToIdle(state)
		d.activeCount.Add(-1)
		d.metrics.SetActiveVehicles(int(d.activeCount.Load()))

		evt := events.NewEvent(events.DriveDiscardedEvent{
			VIN:         vin,
			DriveID:     driveID,
			DiscardedAt: d.now(),
		})
		if err := d.bus.Publish(d.ctx, evt); err != nil {
			d.logger.Error("failed to publish DriveDiscardedEvent",
				slog.String("vin", redactVIN(vin)),
				slog.String("error", err.Error()),
			)
		}
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
