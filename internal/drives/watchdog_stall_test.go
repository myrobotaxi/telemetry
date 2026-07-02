package drives

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// countingWatchdogMetrics records which watchdog end-cause fired so the
// tests can distinguish the stall / duration-cap paths (MYR-160) from
// the pre-existing silence path.
type countingWatchdogMetrics struct {
	NoopDetectorMetrics

	watchdogEnded    atomic.Int32
	stallEnded       atomic.Int32
	durationCapEnded atomic.Int32
}

func (m *countingWatchdogMetrics) IncWatchdogEnded()    { m.watchdogEnded.Add(1) }
func (m *countingWatchdogMetrics) IncStallEnded()       { m.stallEnded.Add(1) }
func (m *countingWatchdogMetrics) IncDurationCapEnded() { m.durationCapEnded.Add(1) }

// stallTestHarness bundles the injectable-clock detector setup shared
// by the MYR-160 watchdog tests. EndDebounce is deliberately large
// (10s) so (a) the telemetry-silence condition cannot fire when the
// test advances the clock by a few hundred ms and (b) the background
// watchdog ticker (EndDebounce/2 = 5s real time) never ticks during
// the test — every check runs through a manual watchdogTick call,
// keeping the tests deterministic.
type stallTestHarness struct {
	bus     *events.ChannelBus
	d       *Detector
	metrics *countingWatchdogMetrics
	advance func(time.Duration)
}

func newStallTestHarness(t *testing.T, stallTimeout, maxDriveDuration time.Duration) *stallTestHarness {
	t.Helper()

	bus := testBus()
	t.Cleanup(func() { bus.Close(context.Background()) })

	cfg := testConfig()
	cfg.EndDebounce = 10 * time.Second
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0
	cfg.StallTimeout = stallTimeout
	cfg.MaxDriveDuration = maxDriveDuration

	metrics := &countingWatchdogMetrics{}
	d := NewDetector(bus, cfg, testLogger(), metrics, nil)

	var clockMu sync.Mutex
	wallNow := time.Now()
	d.setNow(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return wallNow
	})

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	return &stallTestHarness{
		bus:     bus,
		d:       d,
		metrics: metrics,
		advance: func(by time.Duration) {
			clockMu.Lock()
			wallNow = wallNow.Add(by)
			clockMu.Unlock()
		},
	}
}

// idleStreamFields builds a telemetry frame a parked-but-online car
// emits: gear still reads D (the gear=P frame was missed), speed is 0,
// and there is no location (the 10m MinimumDelta suppresses parked GPS
// jitter).
func idleStreamFields() map[string]events.TelemetryValue {
	return map[string]events.TelemetryValue{
		string(telemetry.FieldGear):  {StringVal: ptr("D")},
		string(telemetry.FieldSpeed): {FloatVal: ptr(0.0)},
	}
}

// TestDetector_WatchdogEndsStalledDrive reproduces the MYR-160 "stuck
// active" failure mode: the car parks but the gear=P frame is missed,
// and the car keeps streaming other fields. The silence watchdog never
// fires (lastTelemetryAt refreshes on every ping) and the gear debounce
// never primes — before the stall condition, the drive stayed open
// indefinitely.
func TestDetector_WatchdogEndsStalledDrive(t *testing.T) {
	const vin = "VIN_STALLED"
	h := newStallTestHarness(t, 300*time.Millisecond, 24*time.Hour)

	startedCh := subscribeTopic(t, h.bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, h.bus, events.TopicDriveEnded)

	eventTime := time.Now()

	// Drive with real movement.
	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime, driveFields("D", 30.0, 33.09, -96.82)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}
	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime.Add(30*time.Second), driveFields("D", 40.0, 33.10, -96.82)))
	time.Sleep(50 * time.Millisecond)

	// The car "parks" without a gear=P frame and keeps streaming idle
	// pings. Advance past StallTimeout while keeping telemetry fresh so
	// the silence condition cannot be the thing that ends the drive.
	h.advance(400 * time.Millisecond)
	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime.Add(31*time.Second), idleStreamFields()))
	time.Sleep(50 * time.Millisecond)

	h.d.watchdogTick()

	evt, ok := waitForEvent(endedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveEndedEvent — stalled drive was not ended")
	}
	ended, ok := evt.Payload.(events.DriveEndedEvent)
	if !ok {
		t.Fatalf("expected DriveEndedEvent, got %T", evt.Payload)
	}
	if ended.VIN != vin {
		t.Errorf("VIN = %q, want %q", ended.VIN, vin)
	}
	if got := h.metrics.stallEnded.Load(); got != 1 {
		t.Errorf("IncStallEnded count = %d, want 1", got)
	}
	if got := h.metrics.watchdogEnded.Load(); got != 0 {
		t.Errorf("IncWatchdogEnded count = %d, want 0 (silence path must not fire — telemetry was flowing)", got)
	}
}

// TestDetector_MovingDriveNotEndedByStall guards against false
// positives: a drive that keeps showing movement (speed > 0 / new route
// points) must survive well past StallTimeout.
func TestDetector_MovingDriveNotEndedByStall(t *testing.T) {
	const vin = "VIN_MOVING"
	h := newStallTestHarness(t, 300*time.Millisecond, 24*time.Hour)

	startedCh := subscribeTopic(t, h.bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, h.bus, events.TopicDriveEnded)

	eventTime := time.Now()
	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime, driveFields("D", 30.0, 33.09, -96.82)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}
	time.Sleep(50 * time.Millisecond)

	// Two stall windows elapse, but each is followed by fresh movement
	// before the tick runs.
	for i := 1; i <= 2; i++ {
		h.advance(400 * time.Millisecond)
		lat := 33.09 + 0.01*float64(i)
		publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime.Add(time.Duration(i)*time.Minute), driveFields("D", 45.0, lat, -96.82)))
		time.Sleep(50 * time.Millisecond)
		h.d.watchdogTick()
	}

	expectNoEvent(t, endedCh, 200*time.Millisecond,
		"a drive with ongoing movement must not be ended by the stall condition")
	if got := h.metrics.stallEnded.Load(); got != 0 {
		t.Errorf("IncStallEnded count = %d, want 0", got)
	}
}

// TestDetector_WatchdogCapsMaxDriveDuration: even with continuous
// movement (a pathological or spoofed signal), a drive older than
// MaxDriveDuration is force-ended. The backstop guarantees no drive
// can stay open indefinitely (MYR-160).
func TestDetector_WatchdogCapsMaxDriveDuration(t *testing.T) {
	const vin = "VIN_MARATHON"
	h := newStallTestHarness(t, time.Hour, 500*time.Millisecond)

	startedCh := subscribeTopic(t, h.bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, h.bus, events.TopicDriveEnded)

	eventTime := time.Now()
	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime, driveFields("D", 30.0, 33.09, -96.82)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}
	time.Sleep(50 * time.Millisecond)

	// Before the cap elapses the drive survives ticks.
	h.advance(200 * time.Millisecond)
	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime.Add(time.Minute), driveFields("D", 40.0, 33.10, -96.82)))
	time.Sleep(50 * time.Millisecond)
	h.d.watchdogTick()
	expectNoEvent(t, endedCh, 100*time.Millisecond, "cap must not fire before MaxDriveDuration")

	// Past the cap — movement is still fresh, silence never happened,
	// yet the drive must end.
	h.advance(400 * time.Millisecond)
	publishTelemetry(t, h.bus, telemetryEvent(vin, eventTime.Add(2*time.Minute), driveFields("D", 40.0, 33.11, -96.82)))
	time.Sleep(50 * time.Millisecond)
	h.d.watchdogTick()

	evt, ok := waitForEvent(endedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveEndedEvent — duration cap did not fire")
	}
	ended, ok := evt.Payload.(events.DriveEndedEvent)
	if !ok {
		t.Fatalf("expected DriveEndedEvent, got %T", evt.Payload)
	}
	if ended.VIN != vin {
		t.Errorf("VIN = %q, want %q", ended.VIN, vin)
	}
	if got := h.metrics.durationCapEnded.Load(); got != 1 {
		t.Errorf("IncDurationCapEnded count = %d, want 1", got)
	}
	if got := h.metrics.stallEnded.Load(); got != 0 {
		t.Errorf("IncStallEnded count = %d, want 0", got)
	}
}

// TestDetector_MicroDriveDiscard_PublishesDriveDiscarded: a discarded
// micro-drive must emit DriveDiscardedEvent (and no DriveEndedEvent) so
// the store can delete the row created on drive.started — otherwise the
// row leaks open with endTime unset (MYR-160).
func TestDetector_MicroDriveDiscard_PublishesDriveDiscarded(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	const vin = "VIN_MICRO_BLIP"

	cfg := testConfig()
	cfg.EndDebounce = 10 * time.Second // manual ticks only (see stallTestHarness doc)
	cfg.MinDuration = 2 * time.Minute
	cfg.MinDistanceMiles = 0.1
	cfg.StallTimeout = 300 * time.Millisecond
	cfg.MaxDriveDuration = 24 * time.Hour

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)

	var clockMu sync.Mutex
	wallNow := time.Now()
	d.setNow(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return wallNow
	})

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)
	discardedCh := subscribeTopic(t, bus, events.TopicDriveDiscarded)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// A gear blip starts a drive that never accumulates duration or
	// distance.
	publishTelemetry(t, bus, telemetryEvent(vin, time.Now(), driveFields("D", 2.0, 33.09, -96.82)))
	evt, ok := waitForEvent(startedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}
	started := evt.Payload.(events.DriveStartedEvent)
	time.Sleep(50 * time.Millisecond)

	// Stall out the blip and tick the watchdog.
	clockMu.Lock()
	wallNow = wallNow.Add(400 * time.Millisecond)
	clockMu.Unlock()
	publishTelemetry(t, bus, telemetryEvent(vin, time.Now(), idleStreamFields()))
	time.Sleep(50 * time.Millisecond)
	d.watchdogTick()

	evt, ok = waitForEvent(discardedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveDiscardedEvent")
	}
	discarded, ok := evt.Payload.(events.DriveDiscardedEvent)
	if !ok {
		t.Fatalf("expected DriveDiscardedEvent, got %T", evt.Payload)
	}
	if discarded.DriveID != started.DriveID {
		t.Errorf("DriveID = %q, want %q", discarded.DriveID, started.DriveID)
	}
	if discarded.VIN != vin {
		t.Errorf("VIN = %q, want %q", discarded.VIN, vin)
	}

	expectNoEvent(t, endedCh, 200*time.Millisecond,
		"a discarded micro-drive must not also publish DriveEndedEvent")
}
