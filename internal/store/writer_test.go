package store

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/geocode"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

// --- test doubles ---

type recordedTelemetryWrite struct {
	VIN    string
	Update VehicleUpdate
}

type recordedStatusWrite struct {
	VIN    string
	Status VehicleStatus
}

type mockVehicleUpdater struct {
	mu             sync.Mutex
	telemetryWrites []recordedTelemetryWrite
	statusWrites    []recordedStatusWrite
	err             error
}

func (m *mockVehicleUpdater) UpdateTelemetry(_ context.Context, vin string, update VehicleUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.telemetryWrites = append(m.telemetryWrites, recordedTelemetryWrite{VIN: vin, Update: update})
	return m.err
}

func (m *mockVehicleUpdater) UpdateStatus(_ context.Context, vin string, status VehicleStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusWrites = append(m.statusWrites, recordedStatusWrite{VIN: vin, Status: status})
	return m.err
}

func (m *mockVehicleUpdater) getTelemetryWrites() []recordedTelemetryWrite {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]recordedTelemetryWrite, len(m.telemetryWrites))
	copy(cp, m.telemetryWrites)
	return cp
}

func (m *mockVehicleUpdater) getStatusWrites() []recordedStatusWrite {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]recordedStatusWrite, len(m.statusWrites))
	copy(cp, m.statusWrites)
	return cp
}

type recordedDriveCreate struct {
	Record DriveRecord
}

type recordedDriveComplete struct {
	DriveID string
	Stats   DriveCompletion
}

type recordedRouteAppend struct {
	DriveID string
	Points  []RoutePointRecord
}

type mockDrivePersister struct {
	mu        sync.Mutex
	creates   []recordedDriveCreate
	completes []recordedDriveComplete
	appends   []recordedRouteAppend
	err       error
}

func (m *mockDrivePersister) Create(_ context.Context, drive DriveRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creates = append(m.creates, recordedDriveCreate{Record: drive})
	return m.err
}

func (m *mockDrivePersister) Complete(_ context.Context, driveID string, stats DriveCompletion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completes = append(m.completes, recordedDriveComplete{DriveID: driveID, Stats: stats})
	return m.err
}

func (m *mockDrivePersister) AppendRoutePoints(_ context.Context, driveID string, points []RoutePointRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appends = append(m.appends, recordedRouteAppend{DriveID: driveID, Points: points})
	return m.err
}

func (m *mockDrivePersister) getCreates() []recordedDriveCreate {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]recordedDriveCreate, len(m.creates))
	copy(cp, m.creates)
	return cp
}

func (m *mockDrivePersister) getCompletes() []recordedDriveComplete {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]recordedDriveComplete, len(m.completes))
	copy(cp, m.completes)
	return cp
}

func (m *mockDrivePersister) getAppends() []recordedRouteAppend {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]recordedRouteAppend, len(m.appends))
	copy(cp, m.appends)
	return cp
}

// --- helpers ---

func newTestBus(t *testing.T) *events.ChannelBus {
	t.Helper()
	bus := events.NewChannelBus(events.BusConfig{
		BufferSize:   64,
		DrainTimeout: 2 * time.Second,
	}, events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func newTestWriter(t *testing.T, bus events.Bus, vehicles vehicleUpdater, drives drivePersister, lookup vinLookup) *Writer {
	t.Helper()
	w := NewWriter(vehicles, drives, lookup, bus, geocode.NoopGeocoder{}, slog.Default(), WriterConfig{
		FlushInterval: 50 * time.Millisecond,
		BatchSize:     1000,
	})
	return w
}

// waitForCondition polls check every 5ms until it returns true or timeout.
func waitForCondition(t *testing.T, timeout time.Duration, check func() bool) { //nolint:unparam // timeout is always 2s currently but may vary
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for condition")
		case <-tick.C:
		}
	}
}

func publishTelemetry(t *testing.T, bus events.Bus, vin string, fields map[string]events.TelemetryValue) {
	t.Helper()
	evt := events.NewEvent(events.VehicleTelemetryEvent{
		VIN:       vin,
		CreatedAt: time.Now(),
		Fields:    fields,
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish telemetry: %v", err)
	}
}

// --- tests ---

func TestWriter_TelemetryFlush(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	lookup := &stubVINLookup{vehicles: map[string]Vehicle{}}

	w := newTestWriter(t, bus, vehicles, drives, lookup)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	speed := 65.0
	publishTelemetry(t, bus, "5YJ3E1EA1NF000001", map[string]events.TelemetryValue{
		string(telemetry.FieldSpeed): {FloatVal: &speed},
	})

	// Wait for flush.
	waitForCondition(t, 2*time.Second, func() bool {
		return len(vehicles.getTelemetryWrites()) > 0
	})

	writes := vehicles.getTelemetryWrites()
	if writes[0].VIN != "5YJ3E1EA1NF000001" {
		t.Errorf("VIN = %q, want 5YJ3E1EA1NF000001", writes[0].VIN)
	}
	if writes[0].Update.Speed == nil || *writes[0].Update.Speed != 65 {
		t.Errorf("Speed = %v, want 65", ptrVal(writes[0].Update.Speed))
	}
}

func TestWriter_Coalescing(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	lookup := &stubVINLookup{vehicles: map[string]Vehicle{}}

	w := newTestWriter(t, bus, vehicles, drives, lookup)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	vin := "5YJ3E1EA1NF000001"

	// Publish two events for the same VIN with different fields.
	speed1 := 45.0
	publishTelemetry(t, bus, vin, map[string]events.TelemetryValue{
		string(telemetry.FieldSpeed): {FloatVal: &speed1},
	})

	speed2 := 72.0
	heading := 180.0
	publishTelemetry(t, bus, vin, map[string]events.TelemetryValue{
		string(telemetry.FieldSpeed):   {FloatVal: &speed2},
		string(telemetry.FieldHeading): {FloatVal: &heading},
	})

	// Wait for flush.
	waitForCondition(t, 2*time.Second, func() bool {
		return len(vehicles.getTelemetryWrites()) > 0
	})

	writes := vehicles.getTelemetryWrites()
	if len(writes) != 1 {
		t.Fatalf("expected 1 coalesced write, got %d", len(writes))
	}

	w0 := writes[0]
	if w0.Update.Speed == nil || *w0.Update.Speed != 72 {
		t.Errorf("Speed = %v, want 72 (latest value wins)", ptrVal(w0.Update.Speed))
	}
	if w0.Update.Heading == nil || *w0.Update.Heading != 180 {
		t.Errorf("Heading = %v, want 180 (from second event)", ptrVal(w0.Update.Heading))
	}
}

func TestWriter_MultipleVehicles(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	lookup := &stubVINLookup{vehicles: map[string]Vehicle{}}

	w := newTestWriter(t, bus, vehicles, drives, lookup)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	speed1 := 45.0
	speed2 := 72.0
	publishTelemetry(t, bus, "VIN_AAA", map[string]events.TelemetryValue{
		string(telemetry.FieldSpeed): {FloatVal: &speed1},
	})
	publishTelemetry(t, bus, "VIN_BBB", map[string]events.TelemetryValue{
		string(telemetry.FieldSpeed): {FloatVal: &speed2},
	})

	waitForCondition(t, 2*time.Second, func() bool {
		return len(vehicles.getTelemetryWrites()) >= 2
	})

	writes := vehicles.getTelemetryWrites()

	vins := map[string]bool{}
	for _, w := range writes {
		vins[w.VIN] = true
	}
	if !vins["VIN_AAA"] || !vins["VIN_BBB"] {
		t.Errorf("expected writes for both VINs, got %v", vins)
	}
}

func TestWriter_DriveStarted(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	lookup := &stubVINLookup{
		vehicles: map[string]Vehicle{
			"5YJ3E1EA1NF000001": {ID: "veh_001", VIN: "5YJ3E1EA1NF000001"},
		},
	}

	w := newTestWriter(t, bus, vehicles, drives, lookup)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	evt := events.NewEvent(events.DriveStartedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_001",
		Location: events.Location{
			Latitude:  33.0975,
			Longitude: -96.8214,
		},
		StartedAt: time.Now(),
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getCreates()) > 0
	})

	creates := drives.getCreates()
	if creates[0].Record.ID != "drive_001" {
		t.Errorf("DriveID = %q, want drive_001", creates[0].Record.ID)
	}
	if creates[0].Record.VehicleID != "veh_001" {
		t.Errorf("VehicleID = %q, want veh_001", creates[0].Record.VehicleID)
	}
}

func TestWriter_DriveEnded(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	lookup := &stubVINLookup{vehicles: map[string]Vehicle{}}

	w := newTestWriter(t, bus, vehicles, drives, lookup)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	now := time.Now()
	evt := events.NewEvent(events.DriveEndedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_001",
		EndedAt: now,
		Stats: events.DriveStats{
			Distance:    12.5,
			Duration:    45 * time.Minute,
			AvgSpeed:    16.7,
			MaxSpeed:    55.0,
			EnergyDelta: 4.2,
			EndLocation: events.Location{
				Latitude:  33.1100,
				Longitude: -96.8300,
			},
			EndChargeLevel: 82,
			RoutePoints: []events.RoutePoint{
				{Latitude: 33.0975, Longitude: -96.8214, Speed: 45.0, Heading: 245.0, Timestamp: now},
			},
		},
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getCompletes()) > 0
	})

	// Verify drive completion.
	completes := drives.getCompletes()
	if completes[0].DriveID != "drive_001" {
		t.Errorf("DriveID = %q, want drive_001", completes[0].DriveID)
	}
	if completes[0].Stats.DistanceMiles != 12.5 {
		t.Errorf("DistanceMiles = %f, want 12.5", completes[0].Stats.DistanceMiles)
	}

	// Verify route points appended.
	appends := drives.getAppends()
	if len(appends) != 1 {
		t.Fatalf("expected 1 route append, got %d", len(appends))
	}
	if len(appends[0].Points) != 1 {
		t.Errorf("route points = %d, want 1", len(appends[0].Points))
	}

	// Verify vehicle status set to parked.
	statusWrites := vehicles.getStatusWrites()
	if len(statusWrites) != 1 {
		t.Fatalf("expected 1 status write, got %d", len(statusWrites))
	}
	if statusWrites[0].Status != VehicleStatusParked {
		t.Errorf("status = %q, want %q", statusWrites[0].Status, VehicleStatusParked)
	}
}

func TestWriter_BatchSizeTriggersFlush(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	lookup := &stubVINLookup{vehicles: map[string]Vehicle{}}

	// Set batch size very low so it triggers before the timer.
	w := NewWriter(vehicles, drives, lookup, bus, geocode.NoopGeocoder{}, slog.Default(), WriterConfig{
		FlushInterval: 10 * time.Second, // very long — should not trigger
		BatchSize:     3,
	})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	speed := 65.0
	for i := range 3 {
		vin := "VIN_" + string(rune('A'+i))
		publishTelemetry(t, bus, vin, map[string]events.TelemetryValue{
			string(telemetry.FieldSpeed): {FloatVal: &speed},
		})
	}

	// The batch should flush quickly after the 3rd event, without waiting
	// for the 10s timer.
	waitForCondition(t, 2*time.Second, func() bool {
		return len(vehicles.getTelemetryWrites()) >= 3
	})
}

func TestMergeUpdate(t *testing.T) {
	t1 := time.Date(2026, 3, 17, 14, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 17, 14, 0, 5, 0, time.UTC)

	dst := &VehicleUpdate{
		Speed:       intPtr(45),
		ChargeLevel: intPtr(90),
		LastUpdated: t1,
	}
	src := &VehicleUpdate{
		Speed:       intPtr(72),
		Heading:     intPtr(180),
		LastUpdated: t2,
	}

	mergeUpdate(dst, src)

	if dst.Speed == nil || *dst.Speed != 72 {
		t.Errorf("Speed = %v, want 72 (overwritten)", ptrVal(dst.Speed))
	}
	if dst.ChargeLevel == nil || *dst.ChargeLevel != 90 {
		t.Errorf("ChargeLevel = %v, want 90 (preserved)", ptrVal(dst.ChargeLevel))
	}
	if dst.Heading == nil || *dst.Heading != 180 {
		t.Errorf("Heading = %v, want 180 (added)", ptrVal(dst.Heading))
	}
	if !dst.LastUpdated.Equal(t2) {
		t.Errorf("LastUpdated = %v, want %v (later time)", dst.LastUpdated, t2)
	}
}

func TestMergeUpdate_OlderTimestampPreserved(t *testing.T) {
	t1 := time.Date(2026, 3, 17, 14, 0, 5, 0, time.UTC) // later
	t2 := time.Date(2026, 3, 17, 14, 0, 0, 0, time.UTC) // earlier

	dst := &VehicleUpdate{LastUpdated: t1}
	src := &VehicleUpdate{LastUpdated: t2}

	mergeUpdate(dst, src)

	if !dst.LastUpdated.Equal(t1) {
		t.Errorf("LastUpdated = %v, want %v (should keep later)", dst.LastUpdated, t1)
	}
}
