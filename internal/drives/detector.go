// Package drives implements a per-vehicle state machine that detects drive
// start/end transitions from vehicle telemetry events. When a drive starts,
// is in progress, or ends, the detector publishes lifecycle events back to
// the event bus. It does not persist anything directly.
package drives

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

// Detector subscribes to vehicle telemetry events and maintains a per-vehicle
// state machine that detects drive start/end transitions. It publishes drive
// lifecycle events (started, updated, ended) back to the bus. The detector
// does not persist anything directly -- downstream event subscribers handle
// that.
type Detector struct {
	bus     events.Bus
	cfg     config.DrivesConfig
	logger  *slog.Logger
	metrics DetectorMetrics

	// states holds per-vehicle drive state. Keyed by VIN.
	// Using sync.Map because vehicles connect/disconnect dynamically and
	// reads vastly outnumber writes (every telemetry tick is a read;
	// new vehicle connections are rare writes).
	states sync.Map // map[string]*vehicleState

	// activeCount tracks the number of vehicles currently driving.
	// Updated atomically on drive start/end to avoid iterating all states.
	activeCount atomic.Int32

	// sub is the telemetry subscription used by Stop to unsubscribe.
	sub events.Subscription

	// ctx is the parent context for debounce timer goroutines.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewDetector creates a Detector. The bus is used both for subscribing to
// telemetry events and publishing drive lifecycle events. Call Start to
// begin processing.
func NewDetector(
	bus events.Bus,
	cfg config.DrivesConfig,
	logger *slog.Logger,
	metrics DetectorMetrics,
) *Detector {
	return &Detector{
		bus:     bus,
		cfg:     cfg,
		logger:  logger,
		metrics: metrics,
	}
}

// Start subscribes to TopicVehicleTelemetry and begins processing telemetry
// events. The provided context governs the lifetime of background goroutines
// (debounce timers). Returns an error if subscribing to the bus fails.
func (d *Detector) Start(ctx context.Context) error {
	d.ctx, d.cancel = context.WithCancel(ctx)

	sub, err := d.bus.Subscribe(events.TopicVehicleTelemetry, d.handleEvent)
	if err != nil {
		d.cancel()
		return fmt.Errorf("drives.Detector.Start: %w", err)
	}
	d.sub = sub

	d.logger.Info("drive detector started")
	return nil
}

// Stop unsubscribes from the bus and cancels all debounce timers.
// Active drives are NOT forcibly ended -- they remain in memory.
func (d *Detector) Stop() error {
	d.cancel()

	// Stop all debounce timers.
	d.states.Range(func(_, value any) bool {
		vs := value.(*vehicleState)
		vs.mu.Lock()
		if vs.debounceTimer != nil {
			vs.debounceTimer.Stop()
			vs.debounceTimer = nil
		}
		vs.mu.Unlock()
		return true
	})

	if err := d.bus.Unsubscribe(d.sub); err != nil {
		return fmt.Errorf("drives.Detector.Stop: %w", err)
	}

	d.logger.Info("drive detector stopped")
	return nil
}

// handleEvent is the bus handler callback. It type-asserts the payload,
// loads or creates per-vehicle state, and dispatches to the appropriate
// state handler.
func (d *Detector) handleEvent(event events.Event) {
	te, ok := event.Payload.(events.VehicleTelemetryEvent)
	if !ok {
		d.logger.Warn("drives.Detector: unexpected payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	d.handleTelemetry(te)
}

// handleTelemetry is the main dispatch function. It extracts the VIN,
// loads or creates per-vehicle state, and dispatches based on drive status.
func (d *Detector) handleTelemetry(te events.VehicleTelemetryEvent) {
	vin := te.VIN
	if vin == "" {
		return
	}

	// Lazy-init vehicle state.
	val, _ := d.states.LoadOrStore(vin, &vehicleState{})
	state := val.(*vehicleState)

	state.mu.Lock()
	defer state.mu.Unlock()

	// Extract gear from the telemetry fields (may be absent).
	gear := extractStringField(te.Fields, telemetry.FieldGear)
	if gear != "" {
		state.lastGear = gear
	}

	// Cache location if present for use when drives start without GPS.
	if loc := extractLocation(te.Fields); loc != nil {
		state.lastLocation = loc
	}

	switch state.status {
	case StatusIdle:
		d.handleIdle(state, vin, te)
	case StatusDriving:
		d.handleDriving(state, vin, te)
	}
}

// extractStringField returns the string value for a telemetry field,
// or empty string if absent or not a string.
func extractStringField(fields map[string]events.TelemetryValue, name telemetry.FieldName) string {
	v, ok := fields[string(name)]
	if !ok || v.StringVal == nil {
		return ""
	}
	return *v.StringVal
}

// extractFloatField returns the float64 value for a telemetry field,
// or 0 if absent or not a float.
func extractFloatField(fields map[string]events.TelemetryValue, name telemetry.FieldName) (float64, bool) {
	v, ok := fields[string(name)]
	if !ok || v.FloatVal == nil {
		return 0, false
	}
	return *v.FloatVal, true
}

// extractLocation returns the Location from the telemetry fields, or nil
// if absent.
func extractLocation(fields map[string]events.TelemetryValue) *events.Location {
	v, ok := fields[string(telemetry.FieldLocation)]
	if !ok || v.LocationVal == nil {
		return nil
	}
	return v.LocationVal
}
