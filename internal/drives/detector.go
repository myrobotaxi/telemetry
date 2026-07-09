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
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// minWatchdogInterval is the floor for the periodic watchdog tick. Even
// for very small EndDebounce values used in tests, we won't churn faster
// than this. In production EndDebounce is 30s, so the watchdog runs at
// 15s. See Detector.watchdogInterval.
const minWatchdogInterval = 100 * time.Millisecond

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

	// reconciler is the hook used on Start to close Drive rows
	// orphaned by a previous restart (MYR-146, hardened in MYR-149:
	// every orphan is ended synchronously — no in-memory reattach).
	// nil disables reconciliation — tests use this; the production
	// constructor in cmd/telemetry-server wires a DriveRepo-backed
	// adapter.
	reconciler OpenDriveLister

	// states holds per-vehicle drive state. Keyed by VIN.
	// Using sync.Map because vehicles connect/disconnect dynamically and
	// reads vastly outnumber writes (every telemetry tick is a read;
	// new vehicle connections are rare writes).
	states sync.Map // map[string]*vehicleState

	// activeCount tracks the number of vehicles currently driving.
	// Updated atomically on drive start/end to avoid iterating all states.
	activeCount atomic.Int32

	// subs holds bus subscriptions used by Stop to unsubscribe.
	subs []events.Subscription

	// ctx is the parent context for debounce timer goroutines.
	ctx    context.Context
	cancel context.CancelFunc

	// now returns the current wall-clock time. Indirected so the watchdog
	// can be tested deterministically without sleeping for EndDebounce.
	// Defaults to time.Now in NewDetector.
	now func() time.Time

	// watchdogWG tracks the background watchdog goroutine so Stop can
	// wait for it to exit before returning.
	watchdogWG sync.WaitGroup
}

// NewDetector creates a Detector. The bus is used both for subscribing to
// telemetry events and publishing drive lifecycle events. Call Start to
// begin processing.
//
// reconciler is the hook used on Start to close Drive rows orphaned
// by a previous restart (MYR-146/MYR-149). Pass nil to disable
// reconciliation (tests use this; production wires a DriveRepo-backed
// adapter). It is a required constructor argument
// rather than an optional functional setter so the dependency fails
// loud at compile time — silently shipping a nil reconciler in prod
// is exactly the regression Option B is meant to prevent.
func NewDetector(
	bus events.Bus,
	cfg config.DrivesConfig,
	logger *slog.Logger,
	metrics DetectorMetrics,
	reconciler OpenDriveLister,
) *Detector {
	return &Detector{
		bus:        bus,
		cfg:        cfg,
		logger:     logger,
		metrics:    metrics,
		reconciler: reconciler,
		now:        time.Now,
	}
}

// setNow overrides the time source used by the watchdog. Test-only seam;
// not exported because no production caller should swap the clock.
func (d *Detector) setNow(fn func() time.Time) {
	d.now = fn
}

// watchdogInterval returns how often the end-condition watchdog runs.
// Half of EndDebounce keeps worst-case end-detection latency at
// 1.5 × EndDebounce, while ensuring we never churn faster than
// minWatchdogInterval even when tests set EndDebounce to a few ms.
func (d *Detector) watchdogInterval() time.Duration {
	iv := d.cfg.EndDebounce / 2
	if iv < minWatchdogInterval {
		iv = minWatchdogInterval
	}
	return iv
}

// Start subscribes to TopicVehicleTelemetry and TopicConnectivity and begins
// processing events. The provided context governs the lifetime of background
// goroutines (debounce timers). Returns an error if subscribing to the bus fails.
func (d *Detector) Start(ctx context.Context) error {
	d.ctx, d.cancel = context.WithCancel(ctx)

	// MYR-146/MYR-149: close orphaned Drive rows BEFORE bus
	// subscription so the DB reaches a consistent state (no open
	// rows) before any telemetry frame can start a fresh drive for
	// the same vehicle. Fails open on DB hiccup.
	d.reconcileOpenDrives(d.ctx)

	telSub, err := d.bus.Subscribe(events.TopicVehicleTelemetry, d.handleEvent)
	if err != nil {
		d.cancel()
		return fmt.Errorf("drives.Detector.Start(telemetry): %w", err)
	}

	connSub, err := d.bus.Subscribe(events.TopicConnectivity, d.handleConnectivityEvent)
	if err != nil {
		d.cancel()
		_ = d.bus.Unsubscribe(telSub)
		return fmt.Errorf("drives.Detector.Start(connectivity): %w", err)
	}

	d.subs = []events.Subscription{telSub, connSub}

	// Start the end-condition watchdog. Tesla stops streaming when the
	// vehicle parks, so a gear=P frame is not guaranteed -- without this
	// watchdog the time.AfterFunc-based debounce path is never primed and
	// the drive stays open forever (the MYR-139 R3a regression). Runs
	// AFTER reconciliation so the first tick observes the reconciled
	// state.
	d.watchdogWG.Add(1)
	go d.runWatchdog()

	d.logger.Info("drive detector started",
		slog.Duration("end_debounce", d.cfg.EndDebounce),
		slog.Duration("watchdog_interval", d.watchdogInterval()),
	)
	return nil
}

// Stop unsubscribes from the bus and cancels all debounce timers.
// Active drives are NOT forcibly ended -- they remain in memory.
func (d *Detector) Stop() error {
	d.cancel()

	// Wait for the watchdog goroutine to exit before tearing down the
	// per-vehicle timers below. Otherwise a watchdog tick could race
	// with this loop and observe nil-but-not-yet-stopped timers.
	d.watchdogWG.Wait()

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

	var firstErr error
	for _, sub := range d.subs {
		if err := d.bus.Unsubscribe(sub); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return fmt.Errorf("drives.Detector.Stop: %w", firstErr)
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

// handleConnectivityEvent is the bus handler callback for connectivity events.
// It type-asserts the payload and dispatches to handleConnectivity.
func (d *Detector) handleConnectivityEvent(event events.Event) {
	ce, ok := event.Payload.(events.ConnectivityEvent)
	if !ok {
		d.logger.Warn("drives.Detector: unexpected connectivity payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	d.handleConnectivity(ce)
}

// handleConnectivity ends any active drive for a vehicle that has
// disconnected. This prevents orphaned drives when the vehicle drops
// off without sending a gear=P transition (e.g. network loss, crash).
func (d *Detector) handleConnectivity(ce events.ConnectivityEvent) {
	if ce.Status != events.StatusDisconnected {
		return
	}

	vin := ce.VIN
	if vin == "" {
		return
	}

	val, ok := d.states.Load(vin)
	if !ok {
		return
	}
	state := val.(*vehicleState)

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.status != StatusDriving {
		return
	}

	d.logger.Info("ending drive on disconnect",
		slog.String("vin", redactVIN(vin)),
	)

	d.endDrive(state, vin)
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

	// Stamp the wall-clock arrival time so the watchdog can detect
	// vehicles that stopped streaming (Tesla goes silent when the car
	// parks and the state machine would otherwise never see a gear=P
	// frame to start the debounce).
	state.lastTelemetryAt = d.now()

	// Extract gear from the telemetry fields (may be absent).
	gear := extractStringField(te.Fields, telemetry.FieldGear)
	if gear != "" {
		state.lastGear = gear
	}

	// Cache location if present for use when drives start without GPS.
	if loc := extractLocation(te.Fields); loc != nil {
		state.lastLocation = loc
	}

	// Cache FSD miles whenever present (including while idle) so startDrive
	// can seed the drive baseline from the most recent value — the
	// gear-change event that starts a drive almost never carries FSD miles
	// because it streams on a much slower cadence than gear.
	if fsd, ok := extractFloatField(te.Fields, telemetry.FieldFSDMiles); ok {
		state.lastFSDMiles = fsd
		state.fsdMilesKnown = true
	}

	// Cache odometer the same way (MYR-157) so startDrive can seed an
	// accurate distance baseline — odometer streams on the same slow
	// cadence as FSD and is usually absent from the gear-change frame.
	if odo, ok := extractFloatField(te.Fields, telemetry.FieldOdometer); ok {
		state.lastOdometer = odo
		state.odometerKnown = true
	}

	// Cache SOC the same way (MYR-207) so startDrive can seed
	// startChargeLevel from the last-known charge — the gear-change frame
	// that starts a drive frequently lacks the charge atomic group, which
	// otherwise persisted startChargeLevel=0. Only cache non-zero values:
	// a literal 0 is exactly the zero-value default we are guarding
	// against, never a plausible parked charge.
	if soc, ok := extractFloatField(te.Fields, telemetry.FieldSOC); ok && soc > 0 {
		state.lastSOC = soc
		state.socKnown = true
	}

	switch state.status {
	case StatusIdle:
		d.handleIdle(state, vin, te)
	case StatusDriving:
		d.handleDriving(state, vin, te)
	}
}
