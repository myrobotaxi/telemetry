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

			d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{})
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

			d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{})
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

func TestDetector_DisconnectEndsDrive(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	cfg.EndDebounce = 200 * time.Millisecond
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{})
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

func TestDetector_DisconnectWhileIdle_NoEffect(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{})
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

	expectNoEvent(t, endedCh, 200*time.Millisecond, "connected event should not end drive")
}

func TestDetector_DisconnectUnknownVIN_NoEffect(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{})
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
				string(telemetry.FieldOriginLocation):  {LocationVal: navOrigin},
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

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

	now := time.Now()

	// Send telemetry with both GPS location and nav origin — GPS should win.
	fields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):            {StringVal: ptr("D")},
		string(telemetry.FieldSpeed):           {FloatVal: ptr(10.0)},
		string(telemetry.FieldLocation):        {LocationVal: &events.Location{Latitude: 32.95, Longitude: -96.73}},
		string(telemetry.FieldOriginLocation):  {LocationVal: &events.Location{Latitude: 33.09, Longitude: -96.82}},
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

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{})
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
		name     string
		lat1     float64
		lon1     float64
		lat2     float64
		lon2     float64
		wantMi   float64
		tolerance float64
	}{
		{
			name:      "same point",
			lat1:      33.09, lon1: -96.82,
			lat2:      33.09, lon2: -96.82,
			wantMi:    0,
			tolerance: 0.001,
		},
		{
			name:      "short distance (Frisco to Plano ~5mi)",
			lat1:      33.15, lon1: -96.82,
			lat2:      33.02, lon2: -96.77,
			wantMi:    9.3, // approximate
			tolerance: 1.0,
		},
		{
			name:      "moderate distance (Dallas to Austin ~182mi great circle)",
			lat1:      32.78, lon1: -96.80,
			lat2:      30.27, lon2: -97.74,
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
		startedAt:     time.Now(),
		startFSDMiles: 100.0,
		lastFSDMiles:  50.0, // counter reset mid-drive
		lastTimestamp:  time.Now().Add(5 * time.Minute),
	}

	stats := calculateStats(drive)
	if stats.FSDMiles != 0 {
		t.Errorf("FSDMiles: got %f, want 0 (clamped on counter reset)", stats.FSDMiles)
	}
}

func TestCalculateStats_ZeroSpeedCount(t *testing.T) {
	drive := &activeDrive{
		startedAt:    time.Now(),
		speedCount:   0,
		speedSum:     0,
		lastTimestamp: time.Now().Add(5 * time.Minute),
	}

	stats := calculateStats(drive)
	if stats.AvgSpeed != 0 {
		t.Errorf("AvgSpeed: got %f, want 0 (no speed samples)", stats.AvgSpeed)
	}
}
