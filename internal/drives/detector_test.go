package drives

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// testLogger returns a no-op logger for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(
		discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError},
	))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// testConfig returns a DrivesConfig with short durations for fast tests.
func testConfig() config.DrivesConfig {
	return config.DrivesConfig{
		MinDuration:      100 * time.Millisecond,
		MinDistanceMiles: 0.01,
		EndDebounce:      50 * time.Millisecond,
		GeocodeTimeout:   time.Second,
	}
}

// testBus creates a ChannelBus suitable for tests.
func testBus() *events.ChannelBus {
	cfg := events.BusConfig{
		BufferSize:   256,
		DrainTimeout: 2 * time.Second,
	}
	return events.NewChannelBus(cfg, events.NoopBusMetrics{}, testLogger())
}

// ptr returns a pointer to the given value.
func ptr[T any](v T) *T { return &v }

// telemetryEvent creates a VehicleTelemetryEvent with the given fields.
func telemetryEvent(vin string, ts time.Time, fields map[string]events.TelemetryValue) events.VehicleTelemetryEvent {
	return events.VehicleTelemetryEvent{
		VIN:       vin,
		CreatedAt: ts,
		Fields:    fields,
	}
}

// gearField returns a telemetry fields map with a gear value.
func gearField(gear string) map[string]events.TelemetryValue {
	return map[string]events.TelemetryValue{
		string(telemetry.FieldGear): {StringVal: ptr(gear)},
	}
}

// driveFields returns a telemetry fields map with gear, speed, and location.
func driveFields(gear string, speed float64, lat, lng float64) map[string]events.TelemetryValue {
	return map[string]events.TelemetryValue{
		string(telemetry.FieldGear):     {StringVal: ptr(gear)},
		string(telemetry.FieldSpeed):    {FloatVal: ptr(speed)},
		string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: lat, Longitude: lng}},
	}
}

// publishTelemetry publishes a VehicleTelemetryEvent through the bus.
func publishTelemetry(t *testing.T, bus events.Bus, te events.VehicleTelemetryEvent) {
	t.Helper()
	evt := events.NewEvent(te)
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("failed to publish telemetry event: %v", err)
	}
}

// publishConnectivity publishes a ConnectivityEvent through the bus.
func publishConnectivity(t *testing.T, bus events.Bus, vin string, status events.ConnectivityStatus) {
	t.Helper()
	evt := events.NewEvent(events.ConnectivityEvent{
		VIN:       vin,
		Status:    status,
		Timestamp: time.Now(),
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("failed to publish connectivity event: %v", err)
	}
}

// eventWaitTimeout is the default timeout for waiting for events in tests.
const eventWaitTimeout = 2 * time.Second

// waitForEvent waits for an event on the channel with eventWaitTimeout.
func waitForEvent(ch <-chan events.Event) (events.Event, bool) {
	select {
	case evt := <-ch:
		return evt, true
	case <-time.After(eventWaitTimeout):
		return events.Event{}, false
	}
}

// expectNoEvent verifies that no event arrives on the channel within the timeout.
func expectNoEvent(t *testing.T, ch <-chan events.Event, timeout time.Duration, msg string) {
	t.Helper()
	select {
	case evt := <-ch:
		t.Fatalf("unexpected event received (%s): topic=%s", msg, evt.Topic)
	case <-time.After(timeout):
		// Good -- no event received.
	}
}

// keepTelemetryAlive starts a goroutine that publishes gear=D telemetry
// for vin every 20ms until the returned stop function is called. Used
// by tests that need to assert NO drive.ended event under conditions
// other than telemetry silence -- without it, the MYR-139 R3a watchdog
// would end the drive after EndDebounce.
//
// The goroutine swallows bus.Publish errors so a teardown race (bus
// closed while the goroutine is in flight) does not fail the test.
func keepTelemetryAlive(bus events.Bus, vin string, start time.Time) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				i++
				ts := start.Add(time.Duration(i) * time.Second)
				lat := 33.09 + 0.001*float64(i)
				evt := events.NewEvent(telemetryEvent(vin, ts, driveFields("D", 30.0, lat, -96.82)))
				_ = bus.Publish(context.Background(), evt)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// subscribeTopic subscribes to a topic and returns a channel that receives events.
func subscribeTopic(t *testing.T, bus events.Bus, topic events.Topic) <-chan events.Event {
	t.Helper()
	ch := make(chan events.Event, 64)
	_, err := bus.Subscribe(topic, func(e events.Event) {
		ch <- e
	})
	if err != nil {
		t.Fatalf("failed to subscribe to %s: %v", topic, err)
	}
	// Subscriptions are cleaned up when bus.Close() is called in the test teardown.
	return ch
}

func TestDetector_IdleToDriving(t *testing.T) {
	tests := []struct {
		name string
		gear string
	}{
		{name: "gear D starts drive", gear: "D"},
		{name: "gear R starts drive", gear: "R"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := testBus()
			defer bus.Close(context.Background())

			d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
			if err := d.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = d.Stop() }()

			startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

			now := time.Now()
			publishTelemetry(t, bus, telemetryEvent("VIN001", now, driveFields(tt.gear, 25.0, 33.09, -96.82)))

			evt, ok := waitForEvent(startedCh)
			if !ok {
				t.Fatal("timed out waiting for DriveStartedEvent")
			}

			payload, ok := evt.Payload.(events.DriveStartedEvent)
			if !ok {
				t.Fatalf("expected DriveStartedEvent, got %T", evt.Payload)
			}
			if payload.VIN != "VIN001" {
				t.Errorf("VIN: got %q, want %q", payload.VIN, "VIN001")
			}
			if payload.Location.Latitude != 33.09 {
				t.Errorf("Latitude: got %f, want 33.09", payload.Location.Latitude)
			}
		})
	}
}

func TestDetector_DrivingToIdle_AfterDebounce(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	// Use a very short debounce so we don't wait long.
	cfg.EndDebounce = 50 * time.Millisecond
	// Use very small minimums so the drive passes.
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)
	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

	now := time.Now()

	// Start driving.
	publishTelemetry(t, bus, telemetryEvent("VIN002", now, driveFields("D", 30.0, 33.09, -96.82)))

	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	// Accumulate a route point at a different location.
	publishTelemetry(t, bus, telemetryEvent("VIN002", now.Add(time.Second), driveFields("D", 50.0, 33.10, -96.83)))

	// Shift to P -- triggers debounce.
	publishTelemetry(t, bus, telemetryEvent("VIN002", now.Add(2*time.Second), driveFields("P", 0, 33.10, -96.83)))

	// Wait for the debounce to fire and DriveEndedEvent to be published.
	evt, ok := waitForEvent(endedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveEndedEvent")
	}

	payload, ok := evt.Payload.(events.DriveEndedEvent)
	if !ok {
		t.Fatalf("expected DriveEndedEvent, got %T", evt.Payload)
	}
	if payload.VIN != "VIN002" {
		t.Errorf("VIN: got %q, want %q", payload.VIN, "VIN002")
	}
}

func TestDetector_DebounceCancellation(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	cfg.EndDebounce = 200 * time.Millisecond

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	now := time.Now()

	// Start driving.
	publishTelemetry(t, bus, telemetryEvent("VIN003", now, driveFields("D", 30.0, 33.09, -96.82)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	// Shift to P -- starts debounce.
	publishTelemetry(t, bus, telemetryEvent("VIN003", now.Add(time.Second), driveFields("P", 0, 33.09, -96.82)))

	// Shift back to D immediately -- cancels debounce before it fires.
	publishTelemetry(t, bus, telemetryEvent("VIN003", now.Add(2*time.Second), driveFields("D", 25.0, 33.10, -96.83)))

	// Keep telemetry flowing so the silent-telemetry watchdog (MYR-139
	// R3a) does not end the drive as a side effect. This test asserts
	// gear=P -> gear=D debounce cancellation specifically.
	stopTel := keepTelemetryAlive(bus, "VIN003", now)
	defer stopTel()

	// The debounce period passes -- drive should NOT have ended.
	expectNoEvent(t, endedCh, 300*time.Millisecond, "drive should continue after debounce cancellation")
}

func TestDetector_MicroDriveFiltering(t *testing.T) {
	tests := []struct {
		name             string
		minDuration      time.Duration
		minDistanceMiles float64
		driveDuration    time.Duration
	}{
		{
			name:             "too short duration",
			minDuration:      5 * time.Second,
			minDistanceMiles: 0,
			driveDuration:    100 * time.Millisecond,
		},
		{
			name:             "too short distance (same location)",
			minDuration:      0,
			minDistanceMiles: 100.0, // 100 miles -- impossible to reach
			driveDuration:    100 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := testBus()
			defer bus.Close(context.Background())

			cfg := testConfig()
			cfg.EndDebounce = 20 * time.Millisecond
			cfg.MinDuration = tt.minDuration
			cfg.MinDistanceMiles = tt.minDistanceMiles

			d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
			if err := d.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = d.Stop() }()

			startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
			endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

			now := time.Now()

			// Start driving.
			publishTelemetry(t, bus, telemetryEvent("VIN_MICRO", now, driveFields("D", 5.0, 33.09, -96.82)))
			if _, ok := waitForEvent(startedCh); !ok {
				t.Fatal("timed out waiting for DriveStartedEvent")
			}

			// Shift to P after short drive.
			publishTelemetry(t, bus, telemetryEvent("VIN_MICRO", now.Add(tt.driveDuration), driveFields("P", 0, 33.09, -96.82)))

			// Wait for debounce -- DriveEndedEvent should NOT be published.
			expectNoEvent(t, endedCh, 300*time.Millisecond, "micro-drive should be discarded")
		})
	}
}

func TestDetector_MultipleVehicles(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	cfg.EndDebounce = 30 * time.Millisecond
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

	now := time.Now()

	// Start driving two vehicles.
	publishTelemetry(t, bus, telemetryEvent("VIN_A", now, driveFields("D", 30.0, 33.09, -96.82)))
	publishTelemetry(t, bus, telemetryEvent("VIN_B", now, driveFields("D", 40.0, 34.00, -97.00)))

	// Collect two DriveStartedEvents.
	vins := make(map[string]bool)
	for i := 0; i < 2; i++ {
		evt, ok := waitForEvent(startedCh)
		if !ok {
			t.Fatalf("timed out waiting for DriveStartedEvent #%d", i+1)
		}
		payload := evt.Payload.(events.DriveStartedEvent)
		vins[payload.VIN] = true
	}

	if !vins["VIN_A"] {
		t.Error("missing DriveStartedEvent for VIN_A")
	}
	if !vins["VIN_B"] {
		t.Error("missing DriveStartedEvent for VIN_B")
	}
}

func TestDetector_NoGearField_NoTransition(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

	now := time.Now()

	// Publish telemetry without a gear field.
	fields := map[string]events.TelemetryValue{
		string(telemetry.FieldSpeed):    {FloatVal: ptr(30.0)},
		string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.09, Longitude: -96.82}},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_NOGEAR", now, fields))

	expectNoEvent(t, startedCh, 200*time.Millisecond, "no gear field should not trigger drive start")
}

func TestDetector_GearN_DoesNotStartDrive(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

	now := time.Now()

	// Gear N should not start a drive.
	publishTelemetry(t, bus, telemetryEvent("VIN_N", now, gearField("N")))

	expectNoEvent(t, startedCh, 200*time.Millisecond, "gear N should not start drive")
}

func TestDetector_GearP_WhileIdle_NoEffect(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	now := time.Now()

	// Gear P while idle should not cause any events.
	publishTelemetry(t, bus, telemetryEvent("VIN_P", now, gearField("P")))

	expectNoEvent(t, startedCh, 200*time.Millisecond, "gear P while idle should not start drive")
	expectNoEvent(t, endedCh, 200*time.Millisecond, "gear P while idle should not end drive")
}

func TestDetector_DriveUpdatedEvents(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	updatedCh := subscribeTopic(t, bus, events.TopicDriveUpdated)

	now := time.Now()

	// Start driving.
	publishTelemetry(t, bus, telemetryEvent("VIN_UPD", now, driveFields("D", 30.0, 33.09, -96.82)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	// The start event's telemetry doesn't produce an update because the
	// state handler calls startDrive, not handleDriving. The first update
	// comes from the next telemetry tick.
	// But the first event IS processed by handleIdle->startDrive, not handleDriving.
	// So we need a second event to get a DriveUpdatedEvent.

	// Send a second telemetry with location.
	publishTelemetry(t, bus, telemetryEvent("VIN_UPD", now.Add(time.Second), driveFields("D", 45.0, 33.10, -96.83)))

	evt, ok := waitForEvent(updatedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveUpdatedEvent")
	}

	payload, ok := evt.Payload.(events.DriveUpdatedEvent)
	if !ok {
		t.Fatalf("expected DriveUpdatedEvent, got %T", evt.Payload)
	}
	if payload.VIN != "VIN_UPD" {
		t.Errorf("VIN: got %q, want %q", payload.VIN, "VIN_UPD")
	}
	if payload.RoutePoint.Speed != 45.0 {
		t.Errorf("Speed: got %f, want 45.0", payload.RoutePoint.Speed)
	}
}

func TestDetector_StatsAccuracy(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	cfg.EndDebounce = 20 * time.Millisecond
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	now := time.Now()

	// Start driving with initial data.
	startFields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):            {StringVal: ptr("D")},
		string(telemetry.FieldSpeed):           {FloatVal: ptr(0.0)},
		string(telemetry.FieldLocation):        {LocationVal: &events.Location{Latitude: 33.0, Longitude: -96.0}},
		string(telemetry.FieldSOC):             {FloatVal: ptr(80.0)},
		string(telemetry.FieldEnergyRemaining): {FloatVal: ptr(50.0)},
		string(telemetry.FieldFSDMiles):        {FloatVal: ptr(100.0)},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_STATS", now, startFields))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	// Drive point 1: moving.
	point1Fields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):            {StringVal: ptr("D")},
		string(telemetry.FieldSpeed):           {FloatVal: ptr(60.0)},
		string(telemetry.FieldLocation):        {LocationVal: &events.Location{Latitude: 33.1, Longitude: -96.1}},
		string(telemetry.FieldSOC):             {FloatVal: ptr(78.0)},
		string(telemetry.FieldEnergyRemaining): {FloatVal: ptr(48.5)},
		string(telemetry.FieldFSDMiles):        {FloatVal: ptr(105.0)},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_STATS", now.Add(3*time.Minute), point1Fields))

	// Drive point 2: faster.
	point2Fields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):            {StringVal: ptr("D")},
		string(telemetry.FieldSpeed):           {FloatVal: ptr(75.0)},
		string(telemetry.FieldLocation):        {LocationVal: &events.Location{Latitude: 33.2, Longitude: -96.2}},
		string(telemetry.FieldSOC):             {FloatVal: ptr(75.0)},
		string(telemetry.FieldEnergyRemaining): {FloatVal: ptr(46.0)},
		string(telemetry.FieldFSDMiles):        {FloatVal: ptr(110.0)},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_STATS", now.Add(6*time.Minute), point2Fields))

	// Stop: shift to P.
	stopFields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):            {StringVal: ptr("P")},
		string(telemetry.FieldSpeed):           {FloatVal: ptr(0.0)},
		string(telemetry.FieldLocation):        {LocationVal: &events.Location{Latitude: 33.2, Longitude: -96.2}},
		string(telemetry.FieldSOC):             {FloatVal: ptr(75.0)},
		string(telemetry.FieldEnergyRemaining): {FloatVal: ptr(46.0)},
		string(telemetry.FieldFSDMiles):        {FloatVal: ptr(110.0)},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_STATS", now.Add(7*time.Minute), stopFields))

	// Wait for debounce to fire.
	evt, ok := waitForEvent(endedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveEndedEvent")
	}

	payload := evt.Payload.(events.DriveEndedEvent)
	stats := payload.Stats

	// Verify max speed tracked correctly.
	if stats.MaxSpeed != 75.0 {
		t.Errorf("MaxSpeed: got %f, want 75.0", stats.MaxSpeed)
	}

	// Verify average speed is reasonable (average of 60 and 75 and 0 samples
	// from handleDriving -- the start event goes through handleIdle so the
	// 0 speed is not counted).
	if stats.AvgSpeed < 30 || stats.AvgSpeed > 80 {
		t.Errorf("AvgSpeed: got %f, want between 30 and 80", stats.AvgSpeed)
	}

	// Verify distance is positive (from haversine).
	if stats.Distance <= 0 {
		t.Errorf("Distance: got %f, want > 0", stats.Distance)
	}

	// Verify charge levels.
	if stats.StartChargeLevel != 80 {
		t.Errorf("StartChargeLevel: got %d, want 80", stats.StartChargeLevel)
	}
	if stats.EndChargeLevel != 75 {
		t.Errorf("EndChargeLevel: got %d, want 75", stats.EndChargeLevel)
	}

	// Verify energy delta.
	expectedEnergy := 50.0 - 46.0
	if stats.EnergyDelta != expectedEnergy {
		t.Errorf("EnergyDelta: got %f, want %f", stats.EnergyDelta, expectedEnergy)
	}

	// Verify FSD miles.
	expectedFSD := 110.0 - 100.0
	if stats.FSDMiles != expectedFSD {
		t.Errorf("FSDMiles: got %f, want %f", stats.FSDMiles, expectedFSD)
	}

	// Verify route points were collected.
	if len(stats.RoutePoints) < 2 {
		t.Errorf("RoutePoints: got %d, want >= 2", len(stats.RoutePoints))
	}

	// Verify start and end locations.
	if stats.StartLocation.Latitude != 33.0 {
		t.Errorf("StartLocation.Lat: got %f, want 33.0", stats.StartLocation.Latitude)
	}
}

// TestDetector_FSDMilesAcrossCadenceMismatch reproduces the real-world field
// cadence: gear streams every 1s but FSD miles only arrives on a much slower
// cadence (60s / 1-mile delta), so the gear-change frame that starts a drive
// carries no FSD value. The detector must seed the FSD baseline from the most
// recent value cached while idle (or, failing that, the first in-drive sample)
// rather than recording zero. Regression test for the bug where FSD miles was
// always reported as 0 because startFSDMiles was never captured.
func TestDetector_FSDMilesAcrossCadenceMismatch(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	cfg.EndDebounce = 20 * time.Millisecond
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	now := time.Now()

	// Idle FSD tick: the vehicle reports an FSD value while parked. This is
	// the baseline the next drive should use.
	idleFields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):     {StringVal: ptr("P")},
		string(telemetry.FieldFSDMiles): {FloatVal: ptr(200.0)},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_FSD", now, idleFields))

	// Drive starts on a gear-change frame that carries NO FSD field — the
	// realistic case given the 1s gear / 60s FSD cadence mismatch.
	startFields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):     {StringVal: ptr("D")},
		string(telemetry.FieldSpeed):    {FloatVal: ptr(0.0)},
		string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.0, Longitude: -96.0}},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_FSD", now.Add(1*time.Second), startFields))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	// A later in-drive frame carries an updated FSD value (next 60s tick).
	midFields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):     {StringVal: ptr("D")},
		string(telemetry.FieldSpeed):    {FloatVal: ptr(60.0)},
		string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.2, Longitude: -96.2}},
		string(telemetry.FieldFSDMiles): {FloatVal: ptr(208.0)},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_FSD", now.Add(61*time.Second), midFields))

	// Park to end the drive, at a distinct location so the route has nonzero
	// distance (needed to exercise the FSD-percentage calculation).
	stopFields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):     {StringVal: ptr("P")},
		string(telemetry.FieldSpeed):    {FloatVal: ptr(0.0)},
		string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.3, Longitude: -96.3}},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_FSD", now.Add(120*time.Second), stopFields))

	evt, ok := waitForEvent(endedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveEndedEvent")
	}
	stats := evt.Payload.(events.DriveEndedEvent).Stats

	// Baseline 200 (from the idle tick) → 208 = 8 FSD miles. Before the fix
	// this was 0 because the gear-change start frame carried no FSD field.
	if want := 8.0; stats.FSDMiles != want {
		t.Errorf("FSDMiles: got %f, want %f", stats.FSDMiles, want)
	}
	if stats.FSDPercentage <= 0 {
		t.Errorf("FSDPercentage: got %f, want > 0", stats.FSDPercentage)
	}
}

// TestDetector_StartChargeAcrossCadenceMismatch is the MYR-207 regression
// test. The charge atomic group streams on a slower cadence than gear, so the
// gear-change frame that starts a drive frequently carries no SOC. The
// detector must seed startChargeLevel from the last-known charge cached while
// idle rather than persisting 0 (which produced the nonsense "0% -> 75%,
// -75% used" drive summaries on production). endChargeLevel behaviour must be
// unchanged. Two scenarios: charge arrives late in the drive, and charge never
// arrives during the drive at all.
func TestDetector_StartChargeAcrossCadenceMismatch(t *testing.T) {
	t.Run("charge arrives late in drive", func(t *testing.T) {
		bus := testBus()
		defer bus.Close(context.Background())

		cfg := testConfig()
		cfg.EndDebounce = 20 * time.Millisecond
		cfg.MinDuration = 0
		cfg.MinDistanceMiles = 0

		d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
		if err := d.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = d.Stop() }()

		startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
		endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

		now := time.Now()

		// Idle charge tick: the vehicle reports SOC=75 while parked. This is
		// the last-known charge the next drive should seed its start from.
		idleFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear): {StringVal: ptr("P")},
			string(telemetry.FieldSOC):  {FloatVal: ptr(75.0)},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC", now, idleFields))

		// Drive starts on a gear-change frame that carries NO SOC field — the
		// realistic case given the gear/charge cadence mismatch.
		startFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear):     {StringVal: ptr("D")},
			string(telemetry.FieldSpeed):    {FloatVal: ptr(0.0)},
			string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.0, Longitude: -96.0}},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC", now.Add(1*time.Second), startFields))
		if _, ok := waitForEvent(startedCh); !ok {
			t.Fatal("timed out waiting for DriveStartedEvent")
		}

		// A later in-drive frame finally carries an updated SOC value.
		midFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear):     {StringVal: ptr("D")},
			string(telemetry.FieldSpeed):    {FloatVal: ptr(60.0)},
			string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.2, Longitude: -96.2}},
			string(telemetry.FieldSOC):      {FloatVal: ptr(70.0)},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC", now.Add(61*time.Second), midFields))

		stopFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear):     {StringVal: ptr("P")},
			string(telemetry.FieldSpeed):    {FloatVal: ptr(0.0)},
			string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.3, Longitude: -96.3}},
			string(telemetry.FieldSOC):      {FloatVal: ptr(70.0)},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC", now.Add(120*time.Second), stopFields))

		evt, ok := waitForEvent(endedCh)
		if !ok {
			t.Fatal("timed out waiting for DriveEndedEvent")
		}
		stats := evt.Payload.(events.DriveEndedEvent).Stats

		// Start charge is the pre-drive last-known value (75), NOT the late
		// in-drive sample (70) and NOT a zero-value default. Before the fix
		// this was 0 because the gear-change start frame carried no SOC.
		if stats.StartChargeLevel != 75 {
			t.Errorf("StartChargeLevel: got %d, want 75", stats.StartChargeLevel)
		}
		// End charge behaviour is unchanged: the most recent in-drive SOC.
		if stats.EndChargeLevel != 70 {
			t.Errorf("EndChargeLevel: got %d, want 70", stats.EndChargeLevel)
		}
	})

	t.Run("charge never arrives during drive", func(t *testing.T) {
		bus := testBus()
		defer bus.Close(context.Background())

		cfg := testConfig()
		cfg.EndDebounce = 20 * time.Millisecond
		cfg.MinDuration = 0
		cfg.MinDistanceMiles = 0

		d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
		if err := d.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = d.Stop() }()

		startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
		endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

		now := time.Now()

		// Idle charge tick establishes the last-known charge (62).
		idleFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear): {StringVal: ptr("P")},
			string(telemetry.FieldSOC):  {FloatVal: ptr(62.0)},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC2", now, idleFields))

		// Entire drive carries no SOC field at all.
		startFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear):     {StringVal: ptr("D")},
			string(telemetry.FieldSpeed):    {FloatVal: ptr(0.0)},
			string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.0, Longitude: -96.0}},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC2", now.Add(1*time.Second), startFields))
		if _, ok := waitForEvent(startedCh); !ok {
			t.Fatal("timed out waiting for DriveStartedEvent")
		}

		midFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear):     {StringVal: ptr("D")},
			string(telemetry.FieldSpeed):    {FloatVal: ptr(55.0)},
			string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.2, Longitude: -96.2}},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC2", now.Add(61*time.Second), midFields))

		stopFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear):     {StringVal: ptr("P")},
			string(telemetry.FieldSpeed):    {FloatVal: ptr(0.0)},
			string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.3, Longitude: -96.3}},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC2", now.Add(120*time.Second), stopFields))

		evt, ok := waitForEvent(endedCh)
		if !ok {
			t.Fatal("timed out waiting for DriveEndedEvent")
		}
		stats := evt.Payload.(events.DriveEndedEvent).Stats

		// With no in-drive SOC ever, both start and end fall back to the
		// pre-drive last-known charge (62) — a plausible, non-zero summary
		// rather than the "0% -> 0%" the bug produced.
		if stats.StartChargeLevel != 62 {
			t.Errorf("StartChargeLevel: got %d, want 62", stats.StartChargeLevel)
		}
		if stats.EndChargeLevel != 62 {
			t.Errorf("EndChargeLevel: got %d, want 62", stats.EndChargeLevel)
		}
	})

	t.Run("cold start seeds from first in-drive sample", func(t *testing.T) {
		bus := testBus()
		defer bus.Close(context.Background())

		cfg := testConfig()
		cfg.EndDebounce = 20 * time.Millisecond
		cfg.MinDuration = 0
		cfg.MinDistanceMiles = 0

		d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
		if err := d.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() { _ = d.Stop() }()

		startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
		endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

		now := time.Now()

		// No idle charge tick: the detector has never seen SOC for this
		// vehicle (e.g. first drive after a restart). The start frame also
		// carries no SOC.
		startFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear):     {StringVal: ptr("D")},
			string(telemetry.FieldSpeed):    {FloatVal: ptr(0.0)},
			string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.0, Longitude: -96.0}},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC3", now, startFields))
		if _, ok := waitForEvent(startedCh); !ok {
			t.Fatal("timed out waiting for DriveStartedEvent")
		}

		// First in-drive SOC sample seeds the start charge lazily.
		midFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear):     {StringVal: ptr("D")},
			string(telemetry.FieldSpeed):    {FloatVal: ptr(55.0)},
			string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.2, Longitude: -96.2}},
			string(telemetry.FieldSOC):      {FloatVal: ptr(58.0)},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC3", now.Add(61*time.Second), midFields))

		stopFields := map[string]events.TelemetryValue{
			string(telemetry.FieldGear):     {StringVal: ptr("P")},
			string(telemetry.FieldSpeed):    {FloatVal: ptr(0.0)},
			string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.3, Longitude: -96.3}},
			string(telemetry.FieldSOC):      {FloatVal: ptr(54.0)},
		}
		publishTelemetry(t, bus, telemetryEvent("VIN_SOC3", now.Add(120*time.Second), stopFields))

		evt, ok := waitForEvent(endedCh)
		if !ok {
			t.Fatal("timed out waiting for DriveEndedEvent")
		}
		stats := evt.Payload.(events.DriveEndedEvent).Stats

		// Start charge is the first in-drive sample (58), not 0.
		if stats.StartChargeLevel != 58 {
			t.Errorf("StartChargeLevel: got %d, want 58", stats.StartChargeLevel)
		}
		if stats.EndChargeLevel != 54 {
			t.Errorf("EndChargeLevel: got %d, want 54", stats.EndChargeLevel)
		}
	})
}

func TestDetector_DisconnectEndsDrive(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	cfg.EndDebounce = 200 * time.Millisecond
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	now := time.Now()

	// Start driving.
	publishTelemetry(t, bus, telemetryEvent("VIN_DC", now, driveFields("D", 30.0, 33.09, -96.82)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	// Accumulate a route point.
	publishTelemetry(t, bus, telemetryEvent("VIN_DC", now.Add(time.Second), driveFields("D", 50.0, 33.10, -96.83)))

	// Vehicle disconnects without shifting to P.
	publishConnectivity(t, bus, "VIN_DC", events.StatusDisconnected)

	// Drive should end immediately (no debounce wait).
	evt, ok := waitForEvent(endedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveEndedEvent after disconnect")
	}

	payload, ok := evt.Payload.(events.DriveEndedEvent)
	if !ok {
		t.Fatalf("expected DriveEndedEvent, got %T", evt.Payload)
	}
	if payload.VIN != "VIN_DC" {
		t.Errorf("VIN: got %q, want %q", payload.VIN, "VIN_DC")
	}
}

// TestDetector_WatchdogEndsDriveWhenTelemetrySilent reproduces MYR-139 R3a:
// a vehicle drives for a minute, then Tesla stops streaming entirely
// without ever sending a gear=P frame. The state machine's gear=P-triggered
// AfterFunc debounce never primes, so without the watchdog the drive would
// stay open forever, leaving the DB row with endTime IS NULL.
//
// The test uses an injectable clock so it doesn't have to sleep for the
// real EndDebounce. We feed driving telemetry, then advance the clock
// past EndDebounce and wait for the watchdog tick to fire.
func TestDetector_WatchdogEndsDriveWhenTelemetrySilent(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	// EndDebounce drives the watchdog interval (half of). 200ms keeps
	// the test under 1s while still exercising the production path.
	cfg.EndDebounce = 200 * time.Millisecond
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)

	// Inject a clock we can fast-forward. The watchdog reads now() to
	// decide whether telemetry has been silent for EndDebounce.
	var clockMu sync.Mutex
	wallNow := time.Now()
	d.setNow(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return wallNow
	})
	advanceClock := func(by time.Duration) {
		clockMu.Lock()
		wallNow = wallNow.Add(by)
		clockMu.Unlock()
	}

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	eventTime := time.Now()

	// Drive for a minute (in event-time): start frame plus several
	// route points. Telemetry is fully gear=D the whole time; we never
	// send gear=P -- this is the bug scenario.
	publishTelemetry(t, bus, telemetryEvent("VIN_SILENT", eventTime, driveFields("D", 30.0, 33.09, -96.82)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	for i := 1; i <= 6; i++ {
		ts := eventTime.Add(time.Duration(i*10) * time.Second)
		lat := 33.09 + 0.001*float64(i)
		publishTelemetry(t, bus, telemetryEvent("VIN_SILENT", ts, driveFields("D", 40.0, lat, -96.82)))
	}

	// Give the bus handler a moment to process all the telemetry events
	// so lastTelemetryAt is set in the vehicle state.
	time.Sleep(50 * time.Millisecond)

	// Confirm no end yet: the gear=P path was never taken and the
	// clock has not advanced, so the watchdog should not act.
	expectNoEvent(t, endedCh, 100*time.Millisecond, "watchdog should not fire before silence cutoff")

	// Now simulate Tesla going silent: advance the wall clock past
	// EndDebounce. The next watchdog tick should observe the silence
	// and end the drive.
	advanceClock(2 * cfg.EndDebounce)

	evt, ok := waitForEvent(endedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveEndedEvent from watchdog")
	}

	payload, ok := evt.Payload.(events.DriveEndedEvent)
	if !ok {
		t.Fatalf("expected DriveEndedEvent, got %T", evt.Payload)
	}
	if payload.VIN != "VIN_SILENT" {
		t.Errorf("VIN: got %q, want %q", payload.VIN, "VIN_SILENT")
	}
	if payload.DriveID == "" {
		t.Error("DriveID should not be empty")
	}
}

// TestDetector_WatchdogIgnoresActiveDrives ensures the watchdog does NOT
// fire while telemetry is still arriving regularly. Regression guard for
// the watchdog reading lastTelemetryAt correctly.
func TestDetector_WatchdogIgnoresActiveDrives(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	cfg.EndDebounce = 200 * time.Millisecond
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	now := time.Now()

	// Start a drive.
	publishTelemetry(t, bus, telemetryEvent("VIN_ACTIVE", now, driveFields("D", 30.0, 33.09, -96.82)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	// Feed telemetry continuously at an interval well below EndDebounce.
	// The watchdog must NOT fire because lastTelemetryAt is fresh.
	stopTel := keepTelemetryAlive(bus, "VIN_ACTIVE", now)
	defer stopTel()

	// Wait longer than EndDebounce -- still no end event.
	expectNoEvent(t, endedCh, 3*cfg.EndDebounce, "watchdog should not end an active drive")
}

func TestDetector_DisconnectWhileIdle_NoEffect(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	// Send telemetry while idle (gear=P), then disconnect.
	publishTelemetry(t, bus, telemetryEvent("VIN_IDLE_DC", time.Now(), gearField("P")))
	publishConnectivity(t, bus, "VIN_IDLE_DC", events.StatusDisconnected)

	expectNoEvent(t, endedCh, 200*time.Millisecond, "disconnect while idle should not end drive")
}

func TestDetector_ConnectedEvent_NoEffect(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	now := time.Now()

	// Start driving.
	publishTelemetry(t, bus, telemetryEvent("VIN_CONN", now, driveFields("D", 30.0, 33.09, -96.82)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	// Connected event should NOT end the drive.
	publishConnectivity(t, bus, "VIN_CONN", events.StatusConnected)

	// Keep telemetry flowing so the silent-telemetry watchdog (MYR-139
	// R3a) does not end the drive as a side effect. This test asserts
	// connected-event behaviour specifically.
	stopTel := keepTelemetryAlive(bus, "VIN_CONN", now)
	defer stopTel()

	expectNoEvent(t, endedCh, 200*time.Millisecond, "connected event should not end drive")
}

func TestDetector_DisconnectUnknownVIN_NoEffect(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	// Disconnect for a VIN we have never seen.
	publishConnectivity(t, bus, "VIN_UNKNOWN", events.StatusDisconnected)

	expectNoEvent(t, endedCh, 200*time.Millisecond, "disconnect for unknown VIN should have no effect")
}

func TestDetector_StartStop(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestDetector_StartLocationFallback(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

	now := time.Now()

	// Send a telemetry event with location but no drive gear (caches location).
	locFields := map[string]events.TelemetryValue{
		string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.09, Longitude: -96.82}},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_LOC", now, locFields))

	// Now send gear D without location -- should use cached location.
	publishTelemetry(t, bus, telemetryEvent("VIN_LOC", now.Add(time.Second), gearField("D")))

	evt, ok := waitForEvent(startedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	payload := evt.Payload.(events.DriveStartedEvent)
	if payload.Location.Latitude != 33.09 {
		t.Errorf("StartLocation.Lat: got %f, want 33.09 (from cached location)", payload.Location.Latitude)
	}
}

func TestDetector_EmptyVIN_Ignored(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

	// Empty VIN should be silently ignored.
	publishTelemetry(t, bus, telemetryEvent("", time.Now(), gearField("D")))

	expectNoEvent(t, startedCh, 200*time.Millisecond, "empty VIN should be ignored")
}

func TestDetector_ConcurrentDriveEvents(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	cfg.EndDebounce = 20 * time.Millisecond
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	now := time.Now()
	vehicleCount := 5

	// Start all vehicles concurrently.
	var wg sync.WaitGroup
	for i := 0; i < vehicleCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			vin := fmt.Sprintf("VIN_CONC_%d", idx)
			lat := 33.0 + float64(idx)*0.1
			publishTelemetry(t, bus, telemetryEvent(vin, now, driveFields("D", 30.0, lat, -96.0)))
		}(i)
	}
	wg.Wait()

	// Collect all DriveStartedEvents.
	started := make(map[string]bool)
	for i := 0; i < vehicleCount; i++ {
		evt, ok := waitForEvent(startedCh)
		if !ok {
			t.Fatalf("timed out waiting for DriveStartedEvent #%d", i+1)
		}
		payload := evt.Payload.(events.DriveStartedEvent)
		started[payload.VIN] = true
	}

	if len(started) != vehicleCount {
		t.Errorf("started %d vehicles, want %d", len(started), vehicleCount)
	}

	// Stop all vehicles.
	for i := 0; i < vehicleCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			vin := fmt.Sprintf("VIN_CONC_%d", idx)
			lat := 33.0 + float64(idx)*0.1
			publishTelemetry(t, bus, telemetryEvent(vin, now.Add(time.Minute), driveFields("P", 0, lat, -96.0)))
		}(i)
	}
	wg.Wait()

	// Collect all DriveEndedEvents.
	ended := make(map[string]bool)
	for i := 0; i < vehicleCount; i++ {
		evt, ok := waitForEvent(endedCh)
		if !ok {
			t.Fatalf("timed out waiting for DriveEndedEvent #%d (got %d so far)", i+1, len(ended))
		}
		payload := evt.Payload.(events.DriveEndedEvent)
		ended[payload.VIN] = true
	}

	if len(ended) != vehicleCount {
		t.Errorf("ended %d vehicles, want %d", len(ended), vehicleCount)
	}
}

func TestExtractLocation_IgnoresOriginLocation(t *testing.T) {
	gpsLoc := &events.Location{Latitude: 32.95, Longitude: -96.73}
	navOrigin := &events.Location{Latitude: 33.09, Longitude: -96.82}

	tests := []struct {
		name   string
		fields map[string]events.TelemetryValue
		want   *events.Location
	}{
		{
			name: "both location and originLocation present, returns GPS location",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldLocation):       {LocationVal: gpsLoc},
				string(telemetry.FieldOriginLocation): {LocationVal: navOrigin},
			},
			want: gpsLoc,
		},
		{
			name: "only originLocation present, returns nil",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldOriginLocation): {LocationVal: navOrigin},
			},
			want: nil,
		},
		{
			name: "only GPS location present, returns GPS location",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldLocation): {LocationVal: gpsLoc},
			},
			want: gpsLoc,
		},
		{
			name:   "neither location present, returns nil",
			fields: map[string]events.TelemetryValue{},
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLocation(tt.fields)
			if tt.want == nil {
				if got != nil {
					t.Errorf("extractLocation: got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("extractLocation: got nil, want non-nil")
			}
			if got.Latitude != tt.want.Latitude || got.Longitude != tt.want.Longitude {
				t.Errorf("extractLocation: got (%f, %f), want (%f, %f)",
					got.Latitude, got.Longitude, tt.want.Latitude, tt.want.Longitude)
			}
		})
	}
}

func TestDetector_DriveStartUsesGPSNotNavOrigin(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

	now := time.Now()

	// Send telemetry with both GPS location and nav origin — GPS should win.
	fields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):           {StringVal: ptr("D")},
		string(telemetry.FieldSpeed):          {FloatVal: ptr(10.0)},
		string(telemetry.FieldLocation):       {LocationVal: &events.Location{Latitude: 32.95, Longitude: -96.73}},
		string(telemetry.FieldOriginLocation): {LocationVal: &events.Location{Latitude: 33.09, Longitude: -96.82}},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_NAV", now, fields))

	evt, ok := waitForEvent(startedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	payload := evt.Payload.(events.DriveStartedEvent)
	if payload.Location.Latitude != 32.95 {
		t.Errorf("DriveStartedEvent.Location.Latitude: got %f, want 32.95 (GPS, not nav origin 33.09)",
			payload.Location.Latitude)
	}
	if payload.Location.Longitude != -96.73 {
		t.Errorf("DriveStartedEvent.Location.Longitude: got %f, want -96.73 (GPS, not nav origin -96.82)",
			payload.Location.Longitude)
	}
}

func TestDetector_OriginLocationOnlyDoesNotCacheLocation(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

	now := time.Now()

	// Send telemetry with ONLY originLocation (no GPS) — should NOT cache.
	navFields := map[string]events.TelemetryValue{
		string(telemetry.FieldOriginLocation): {LocationVal: &events.Location{Latitude: 33.09, Longitude: -96.82}},
	}
	publishTelemetry(t, bus, telemetryEvent("VIN_NAV2", now, navFields))

	// Now start a drive without any location — should have zero-value location
	// because originLocation should not have been cached.
	publishTelemetry(t, bus, telemetryEvent("VIN_NAV2", now.Add(time.Second), gearField("D")))

	evt, ok := waitForEvent(startedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	payload := evt.Payload.(events.DriveStartedEvent)
	if payload.Location.Latitude != 0 || payload.Location.Longitude != 0 {
		t.Errorf("DriveStartedEvent.Location: got (%f, %f), want (0, 0) — originLocation should not be used as fallback",
			payload.Location.Latitude, payload.Location.Longitude)
	}
}

func TestHaversine(t *testing.T) {
	tests := []struct {
		name      string
		lat1      float64
		lon1      float64
		lat2      float64
		lon2      float64
		wantMi    float64
		tolerance float64
	}{
		{
			name: "same point",
			lat1: 33.09, lon1: -96.82,
			lat2: 33.09, lon2: -96.82,
			wantMi:    0,
			tolerance: 0.001,
		},
		{
			name: "short distance (Frisco to Plano ~5mi)",
			lat1: 33.15, lon1: -96.82,
			lat2: 33.02, lon2: -96.77,
			wantMi:    9.3, // approximate
			tolerance: 1.0,
		},
		{
			name: "moderate distance (Dallas to Austin ~182mi great circle)",
			lat1: 32.78, lon1: -96.80,
			lat2: 30.27, lon2: -97.74,
			wantMi:    182.0,
			tolerance: 5.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := haversine(tt.lat1, tt.lon1, tt.lat2, tt.lon2)
			diff := got - tt.wantMi
			if diff < 0 {
				diff = -diff
			}
			if diff > tt.tolerance {
				t.Errorf("haversine(%f,%f → %f,%f) = %f mi, want ~%f mi (±%f)",
					tt.lat1, tt.lon1, tt.lat2, tt.lon2, got, tt.wantMi, tt.tolerance)
			}
		})
	}
}

func TestTotalDistance(t *testing.T) {
	tests := []struct {
		name   string
		points []events.RoutePoint
		want   float64
		tol    float64
	}{
		{
			name:   "no points",
			points: nil,
			want:   0,
			tol:    0,
		},
		{
			name:   "one point",
			points: []events.RoutePoint{{Latitude: 33.0, Longitude: -96.0}},
			want:   0,
			tol:    0,
		},
		{
			name: "two points",
			points: []events.RoutePoint{
				{Latitude: 33.0, Longitude: -96.0},
				{Latitude: 33.1, Longitude: -96.1},
			},
			want: 8.8, // ~8.8 miles
			tol:  1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := totalDistance(tt.points)
			diff := got - tt.want
			if diff < 0 {
				diff = -diff
			}
			if diff > tt.tol {
				t.Errorf("totalDistance: got %f, want ~%f (±%f)", got, tt.want, tt.tol)
			}
		})
	}
}

func TestRedactVIN(t *testing.T) {
	tests := []struct {
		vin  string
		want string
	}{
		{vin: "5YJ3E1EA1NF000001", want: "***0001"},
		{vin: "ABCD", want: "ABCD"},
		{vin: "AB", want: "AB"},
		{vin: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.vin, func(t *testing.T) {
			got := redactVIN(tt.vin)
			if got != tt.want {
				t.Errorf("redactVIN(%q) = %q, want %q", tt.vin, got, tt.want)
			}
		})
	}
}

func TestCalculateStats_FSDClampedToZero(t *testing.T) {
	drive := &activeDrive{
		startedAt:      time.Now(),
		startFSDMiles:  100.0,
		lastFSDMiles:   50.0, // counter reset mid-drive
		fsdBaselineSet: true,
		lastTimestamp:  time.Now().Add(5 * time.Minute),
	}

	stats := calculateStats(drive)
	if stats.FSDMiles != 0 {
		t.Errorf("FSDMiles: got %f, want 0 (clamped on counter reset)", stats.FSDMiles)
	}
}

// TestCalculateStats_FSDBaselineUnset verifies that when no FSD value was ever
// observed for a drive, FSD miles is reported as zero rather than the full
// cumulative "miles since reset" counter.
func TestCalculateStats_FSDBaselineUnset(t *testing.T) {
	drive := &activeDrive{
		startedAt:     time.Now(),
		lastFSDMiles:  5000.0, // cumulative counter — must NOT be reported as-is
		lastTimestamp: time.Now().Add(5 * time.Minute),
		// fsdBaselineSet defaults to false.
	}

	stats := calculateStats(drive)
	if stats.FSDMiles != 0 {
		t.Errorf("FSDMiles: got %f, want 0 (no baseline observed)", stats.FSDMiles)
	}
}

func TestCalculateStats_ZeroSpeedCount(t *testing.T) {
	drive := &activeDrive{
		startedAt:     time.Now(),
		speedCount:    0,
		speedSum:      0,
		lastTimestamp: time.Now().Add(5 * time.Minute),
	}

	stats := calculateStats(drive)
	if stats.AvgSpeed != 0 {
		t.Errorf("AvgSpeed: got %f, want 0 (no speed samples)", stats.AvgSpeed)
	}
}

// MYR-157: drive distance is derived from the odometer delta (an accurate
// cumulative counter), not the GPS haversine of sparse route points. The
// route points here span a large GPS distance to prove the odometer wins.
func TestCalculateStats_DistanceFromOdometer(t *testing.T) {
	drive := &activeDrive{
		startedAt:           time.Now(),
		lastTimestamp:       time.Now().Add(5 * time.Minute),
		startOdometer:       1000.0,
		lastOdometer:        1002.0, // odometer delta = 2.0 mi
		odometerBaselineSet: true,
		routePoints: []events.RoutePoint{
			{Latitude: 33.0, Longitude: -96.0},
			{Latitude: 33.5, Longitude: -96.5}, // ~45 mi of GPS — must be ignored
		},
	}
	stats := calculateStats(drive)
	if stats.Distance != 2.0 {
		t.Errorf("Distance: got %f, want 2.0 (odometer delta, not GPS)", stats.Distance)
	}
}

// MYR-157: with no odometer baseline observed, distance falls back to the
// GPS haversine sum (legacy behavior).
func TestCalculateStats_DistanceFallsBackToGPS(t *testing.T) {
	drive := &activeDrive{
		startedAt:     time.Now(),
		lastTimestamp: time.Now().Add(5 * time.Minute),
		// odometerBaselineSet defaults false → GPS fallback.
		routePoints: []events.RoutePoint{
			{Latitude: 33.0, Longitude: -96.0},
			{Latitude: 33.01, Longitude: -96.0},
		},
	}
	gps := totalDistance(drive.routePoints)
	stats := calculateStats(drive)
	if gps <= 0 || stats.Distance != gps {
		t.Errorf("Distance: got %f, want GPS fallback %f (>0)", stats.Distance, gps)
	}
}

// MYR-157: a mid-drive odometer reset (delta ≤ 0) falls back to GPS rather
// than reporting a negative/zero distance.
func TestCalculateStats_DistanceFallsBackOnOdometerReset(t *testing.T) {
	drive := &activeDrive{
		startedAt:           time.Now(),
		lastTimestamp:       time.Now().Add(5 * time.Minute),
		startOdometer:       2000.0,
		lastOdometer:        1.0, // reset mid-drive → non-positive delta
		odometerBaselineSet: true,
		routePoints: []events.RoutePoint{
			{Latitude: 33.0, Longitude: -96.0},
			{Latitude: 33.01, Longitude: -96.0},
		},
	}
	gps := totalDistance(drive.routePoints)
	stats := calculateStats(drive)
	if stats.Distance != gps {
		t.Errorf("Distance: got %f, want GPS fallback %f on odometer reset", stats.Distance, gps)
	}
}

// MYR-157: FSD% is clamped to ≤100% — guards the >100% (e.g. 176%) that
// previously surfaced when FSD miles were divided by an undercounting
// distance.
func TestCalculateStats_FSDPercentageClampedTo100(t *testing.T) {
	drive := &activeDrive{
		startedAt:           time.Now(),
		lastTimestamp:       time.Now().Add(5 * time.Minute),
		startFSDMiles:       100.0,
		lastFSDMiles:        102.0, // FSD delta = 2.0
		fsdBaselineSet:      true,
		startOdometer:       500.0,
		lastOdometer:        501.0, // distance = 1.0 → 200% pre-clamp
		odometerBaselineSet: true,
	}
	stats := calculateStats(drive)
	if stats.FSDPercentage != 100.0 {
		t.Errorf("FSDPercentage: got %f, want 100 (clamped)", stats.FSDPercentage)
	}
}

// MYR-157: a normal FSD share computed against the odometer distance.
func TestCalculateStats_FSDPercentageFromOdometer(t *testing.T) {
	drive := &activeDrive{
		startedAt:           time.Now(),
		lastTimestamp:       time.Now().Add(5 * time.Minute),
		startFSDMiles:       100.0,
		lastFSDMiles:        101.0, // FSD = 1.0
		fsdBaselineSet:      true,
		startOdometer:       500.0,
		lastOdometer:        502.0, // distance = 2.0
		odometerBaselineSet: true,
	}
	stats := calculateStats(drive)
	if stats.Distance != 2.0 {
		t.Errorf("Distance: got %f, want 2.0", stats.Distance)
	}
	if stats.FSDPercentage != 50.0 {
		t.Errorf("FSDPercentage: got %f, want 50", stats.FSDPercentage)
	}
}
