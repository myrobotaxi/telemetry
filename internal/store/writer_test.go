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

func newTestWriter(t *testing.T, bus events.Bus, vehicles vehicleUpdater, drives drivePersister, lookup vinIDLookup) *Writer {
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
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}

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
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}

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
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}

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
	lookup := &stubIDLookup{
		pairs: map[string]struct{ id, userID string }{
			"5YJ3E1EA1NF000001": {id: "veh_001", userID: "user_001"},
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
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}

	w := newTestWriter(t, bus, vehicles, drives, lookup)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	now := time.Now()

	// Buffer a route point via DriveUpdatedEvent (simulates mid-drive GPS).
	// handleDriveEnded flushes the route buffer, so we need data in it.
	updateEvt := events.NewEvent(events.DriveUpdatedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_001",
		RoutePoint: events.RoutePoint{
			Latitude: 33.0975, Longitude: -96.8214, Speed: 45.0, Heading: 245.0, Timestamp: now,
		},
	})
	if err := bus.Publish(context.Background(), updateEvt); err != nil {
		t.Fatalf("publish DriveUpdatedEvent: %v", err)
	}

	// Small delay to ensure the update event is processed before drive ends.
	time.Sleep(20 * time.Millisecond)

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

	// Verify route points flushed from buffer on drive end.
	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getAppends()) > 0
	})
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
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}

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

func TestWriter_DriveUpdated_FlushOnSize(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}

	w := NewWriter(vehicles, drives, lookup, bus, geocode.NoopGeocoder{}, slog.Default(), WriterConfig{
		FlushInterval: 50 * time.Millisecond,
		BatchSize:     1000,
		RouteBuffer: RouteBufferConfig{
			FlushInterval: 10 * time.Second,
			FlushSize:     3,
		},
	})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	now := time.Now()
	for i := range 3 {
		evt := events.NewEvent(events.DriveUpdatedEvent{
			VIN:     "5YJ3E1EA1NF000001",
			DriveID: "drive_001",
			RoutePoint: events.RoutePoint{
				Latitude:  33.0975 + float64(i)*0.001,
				Longitude: -96.8214,
				Speed:     45.0,
				Heading:   245.0,
				Timestamp: now.Add(time.Duration(i) * time.Second),
			},
		})
		if err := bus.Publish(context.Background(), evt); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getAppends()) > 0
	})

	appends := drives.getAppends()
	if appends[0].DriveID != "drive_001" {
		t.Errorf("DriveID = %q, want drive_001", appends[0].DriveID)
	}
	if len(appends[0].Points) != 3 {
		t.Errorf("route points = %d, want 3", len(appends[0].Points))
	}
}

func TestWriter_DriveUpdated_FlushOnTimer(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}

	w := NewWriter(vehicles, drives, lookup, bus, geocode.NoopGeocoder{}, slog.Default(), WriterConfig{
		FlushInterval: 50 * time.Millisecond,
		BatchSize:     1000,
		RouteBuffer: RouteBufferConfig{
			FlushInterval: 100 * time.Millisecond,
			FlushSize:     1000,
		},
	})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	now := time.Now()
	evt := events.NewEvent(events.DriveUpdatedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive_002",
		RoutePoint: events.RoutePoint{
			Latitude:  33.0975,
			Longitude: -96.8214,
			Speed:     45.0,
			Heading:   245.0,
			Timestamp: now,
		},
	})
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getAppends()) > 0
	})

	appends := drives.getAppends()
	if appends[0].DriveID != "drive_002" {
		t.Errorf("DriveID = %q, want drive_002", appends[0].DriveID)
	}
	if len(appends[0].Points) != 1 {
		t.Errorf("route points = %d, want 1", len(appends[0].Points))
	}
}

func TestWriter_DriveEnded_FlushesRemainingBufferedPoints(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}

	w := NewWriter(vehicles, drives, lookup, bus, geocode.NoopGeocoder{}, slog.Default(), WriterConfig{
		FlushInterval: 50 * time.Millisecond,
		BatchSize:     1000,
		RouteBuffer: RouteBufferConfig{
			FlushInterval: 10 * time.Second,
			FlushSize:     1000,
		},
	})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	now := time.Now()
	driveID := "drive_003"

	for i := range 2 {
		evt := events.NewEvent(events.DriveUpdatedEvent{
			VIN:     "5YJ3E1EA1NF000001",
			DriveID: driveID,
			RoutePoint: events.RoutePoint{
				Latitude:  33.0975 + float64(i)*0.001,
				Longitude: -96.8214,
				Speed:     45.0,
				Heading:   245.0,
				Timestamp: now.Add(time.Duration(i) * time.Second),
			},
		})
		if err := bus.Publish(context.Background(), evt); err != nil {
			t.Fatalf("publish updated: %v", err)
		}
	}

	time.Sleep(50 * time.Millisecond)

	if len(drives.getAppends()) > 0 {
		t.Fatal("expected no appends before drive ended")
	}

	endEvt := events.NewEvent(events.DriveEndedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: driveID,
		EndedAt: now.Add(5 * time.Minute),
		Stats: events.DriveStats{
			Distance:    2.5,
			Duration:    5 * time.Minute,
			AvgSpeed:    30.0,
			MaxSpeed:    45.0,
			EnergyDelta: 1.0,
			EndLocation: events.Location{
				Latitude:  33.1000,
				Longitude: -96.8300,
			},
			EndChargeLevel: 90,
			RoutePoints: []events.RoutePoint{
				{Latitude: 33.0975, Longitude: -96.8214, Speed: 45.0, Heading: 245.0, Timestamp: now},
				{Latitude: 33.0985, Longitude: -96.8214, Speed: 45.0, Heading: 245.0, Timestamp: now.Add(time.Second)},
			},
		},
	})
	if err := bus.Publish(context.Background(), endEvt); err != nil {
		t.Fatalf("publish ended: %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getCompletes()) > 0
	})

	appends := drives.getAppends()
	if len(appends) != 1 {
		t.Fatalf("expected 1 append call from drive end flush, got %d", len(appends))
	}
	if appends[0].DriveID != driveID {
		t.Errorf("DriveID = %q, want %q", appends[0].DriveID, driveID)
	}
	if len(appends[0].Points) != 2 {
		t.Errorf("route points = %d, want 2", len(appends[0].Points))
	}
}

func TestWriter_DriveUpdated_MultipleDrivers(t *testing.T) {
	bus := newTestBus(t)
	vehicles := &mockVehicleUpdater{}
	drives := &mockDrivePersister{}
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}

	w := NewWriter(vehicles, drives, lookup, bus, geocode.NoopGeocoder{}, slog.Default(), WriterConfig{
		FlushInterval: 50 * time.Millisecond,
		BatchSize:     1000,
		RouteBuffer: RouteBufferConfig{
			FlushInterval: 10 * time.Second,
			FlushSize:     2,
		},
	})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = w.Stop() }()

	now := time.Now()

	for _, driveID := range []string{"drive_A", "drive_B"} {
		for i := range 2 {
			evt := events.NewEvent(events.DriveUpdatedEvent{
				VIN:     "VIN_" + driveID,
				DriveID: driveID,
				RoutePoint: events.RoutePoint{
					Latitude:  33.0 + float64(i)*0.001,
					Longitude: -96.0,
					Speed:     50.0,
					Heading:   180.0,
					Timestamp: now.Add(time.Duration(i) * time.Second),
				},
			})
			if err := bus.Publish(context.Background(), evt); err != nil {
				t.Fatalf("publish: %v", err)
			}
		}
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return len(drives.getAppends()) >= 2
	})

	appends := drives.getAppends()
	driveIDs := map[string]bool{}
	for _, a := range appends {
		driveIDs[a.DriveID] = true
		if len(a.Points) != 2 {
			t.Errorf("drive %s: route points = %d, want 2", a.DriveID, len(a.Points))
		}
	}
	if !driveIDs["drive_A"] || !driveIDs["drive_B"] {
		t.Errorf("expected appends for both drives, got %v", driveIDs)
	}
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

func TestMergeUpdate_ClearFields(t *testing.T) {
	t1 := time.Date(2026, 3, 17, 14, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 17, 14, 0, 5, 0, time.UTC)

	tests := []struct {
		name      string
		dst       *VehicleUpdate
		src       *VehicleUpdate
		wantClear []string
	}{
		{
			name: "ClearFields appended from src",
			dst: &VehicleUpdate{
				Speed:       intPtr(45),
				LastUpdated: t1,
			},
			src: &VehicleUpdate{
				ClearFields: []string{"destinationName", "etaMinutes"},
				LastUpdated: t2,
			},
			wantClear: []string{"destinationName", "etaMinutes"},
		},
		{
			name: "ClearFields from both dst and src merged",
			dst: &VehicleUpdate{
				ClearFields: []string{"originLatitude"},
				LastUpdated: t1,
			},
			src: &VehicleUpdate{
				ClearFields: []string{"destinationLatitude"},
				LastUpdated: t2,
			},
			wantClear: []string{"originLatitude", "destinationLatitude"},
		},
		{
			name: "empty src ClearFields preserves dst ClearFields",
			dst: &VehicleUpdate{
				ClearFields: []string{"etaMinutes"},
				LastUpdated: t1,
			},
			src: &VehicleUpdate{
				Speed:       intPtr(72),
				LastUpdated: t2,
			},
			wantClear: []string{"etaMinutes"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mergeUpdate(tt.dst, tt.src)

			if len(tt.dst.ClearFields) != len(tt.wantClear) {
				t.Fatalf("ClearFields = %v, want %v", tt.dst.ClearFields, tt.wantClear)
			}
			for i := range tt.dst.ClearFields {
				if tt.dst.ClearFields[i] != tt.wantClear[i] {
					t.Errorf("ClearFields[%d] = %q, want %q", i, tt.dst.ClearFields[i], tt.wantClear[i])
				}
			}
		})
	}
}

func TestMergeUpdate_NavFields(t *testing.T) {
	t1 := time.Date(2026, 3, 17, 14, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 17, 14, 0, 5, 0, time.UTC)

	destName := "Home"
	destLat := 33.0975
	destLng := -96.8214
	origLat := 33.05
	origLng := -96.80
	eta := 15
	dist := 8.3

	dst := &VehicleUpdate{
		Speed:       intPtr(45),
		LastUpdated: t1,
	}
	src := &VehicleUpdate{
		DestinationName:      &destName,
		DestinationLatitude:  &destLat,
		DestinationLongitude: &destLng,
		OriginLatitude:       &origLat,
		OriginLongitude:      &origLng,
		EtaMinutes:           &eta,
		TripDistRemaining:    &dist,
		LastUpdated:          t2,
	}

	mergeUpdate(dst, src)

	if dst.DestinationName == nil || *dst.DestinationName != "Home" {
		t.Errorf("DestinationName = %v, want Home", ptrVal(dst.DestinationName))
	}
	if dst.DestinationLatitude == nil || *dst.DestinationLatitude != 33.0975 {
		t.Errorf("DestinationLatitude = %v, want 33.0975", ptrVal(dst.DestinationLatitude))
	}
	if dst.DestinationLongitude == nil || *dst.DestinationLongitude != -96.8214 {
		t.Errorf("DestinationLongitude = %v, want -96.8214", ptrVal(dst.DestinationLongitude))
	}
	if dst.OriginLatitude == nil || *dst.OriginLatitude != 33.05 {
		t.Errorf("OriginLatitude = %v, want 33.05", ptrVal(dst.OriginLatitude))
	}
	if dst.OriginLongitude == nil || *dst.OriginLongitude != -96.80 {
		t.Errorf("OriginLongitude = %v, want -96.80", ptrVal(dst.OriginLongitude))
	}
	if dst.EtaMinutes == nil || *dst.EtaMinutes != 15 {
		t.Errorf("EtaMinutes = %v, want 15", ptrVal(dst.EtaMinutes))
	}
	if dst.TripDistRemaining == nil || *dst.TripDistRemaining != 8.3 {
		t.Errorf("TripDistRemaining = %v, want 8.3", ptrVal(dst.TripDistRemaining))
	}
	if dst.Speed == nil || *dst.Speed != 45 {
		t.Errorf("Speed = %v, want 45 (preserved from dst)", ptrVal(dst.Speed))
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
