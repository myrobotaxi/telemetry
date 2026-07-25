package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// --- fakes ---------------------------------------------------------------

type fakeVehicleReader struct {
	mu    sync.Mutex
	calls int
	state FleetVehicleState
	err   error
}

func (f *fakeVehicleReader) GetVehicle(_ context.Context, _, vin string) (*FleetVehicleState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	s := f.state
	s.VIN = vin
	return &s, nil
}

func (f *fakeVehicleReader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeTokenResolver and stubVehicleOwner are shared with the teardown /
// fleet-config handler tests (vehicle_teardown_handler_test.go,
// fleet_config_handler_test.go).

type fakeStatusUpdater struct {
	mu       sync.Mutex
	statuses []string
	err      error
}

func (f *fakeStatusUpdater) UpdateVehicleStatus(_ context.Context, _, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.statuses = append(f.statuses, status)
	return nil
}

func (f *fakeStatusUpdater) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.statuses))
	copy(out, f.statuses)
	return out
}

const svcTestVIN = "7SAYGDET7TA613795"

func newTestMonitor(
	reader FleetVehicleReader,
	updater VehicleStatusUpdater,
	opts ...ServiceStatusMonitorOption,
) *ServiceStatusMonitor {
	return NewServiceStatusMonitor(
		nil, // bus not needed for direct handleConnectivity tests
		reader,
		&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}},
		&stubVehicleOwner{ownerID: "user-1"},
		updater,
		nil,
		opts...,
	)
}

func connectEvt() events.ConnectivityEvent {
	return events.ConnectivityEvent{VIN: svcTestVIN, Status: events.StatusConnected, Timestamp: time.Now()}
}

func disconnectEvt() events.ConnectivityEvent {
	return events.ConnectivityEvent{VIN: svcTestVIN, Status: events.StatusDisconnected, Timestamp: time.Now()}
}

// --- tests ---------------------------------------------------------------

// TestServiceStatusMonitor_EdgeFiresExactlyOneRead asserts a single
// connectivity edge fires exactly one Fleet API read and, when in_service is
// true, persists status=in_service once.
func TestServiceStatusMonitor_EdgeFiresExactlyOneRead(t *testing.T) {
	reader := &fakeVehicleReader{state: FleetVehicleState{State: "online", InService: true}}
	updater := &fakeStatusUpdater{}
	m := newTestMonitor(reader, updater)

	m.handleConnectivity(context.Background(), connectEvt())

	if got := reader.callCount(); got != 1 {
		t.Fatalf("reader calls = %d, want 1", got)
	}
	if got := updater.written(); len(got) != 1 || got[0] != "in_service" {
		t.Fatalf("persisted statuses = %v, want [in_service]", got)
	}
}

// TestServiceStatusMonitor_DebounceSuppressesRapidSecondRead asserts a rapid
// second edge within the cooldown does NOT fire a second read, but one after
// the cooldown elapses does.
func TestServiceStatusMonitor_DebounceSuppressesRapidSecondRead(t *testing.T) {
	reader := &fakeVehicleReader{state: FleetVehicleState{State: "online", InService: true}}
	updater := &fakeStatusUpdater{}

	base := time.Now()
	clock := base
	m := newTestMonitor(reader, updater,
		WithServiceReadCooldown(45*time.Second),
		withServiceClock(func() time.Time { return clock }),
	)

	// First edge fires a read.
	m.handleConnectivity(context.Background(), connectEvt())
	// Rapid second edge 5s later — within cooldown, suppressed.
	clock = base.Add(5 * time.Second)
	m.handleConnectivity(context.Background(), disconnectEvt())

	if got := reader.callCount(); got != 1 {
		t.Fatalf("after rapid second edge: reader calls = %d, want 1 (debounced)", got)
	}

	// Third edge after the cooldown elapses — fires again.
	clock = base.Add(46 * time.Second)
	m.handleConnectivity(context.Background(), connectEvt())
	if got := reader.callCount(); got != 2 {
		t.Fatalf("after cooldown: reader calls = %d, want 2", got)
	}
}

// TestServiceStatusMonitor_ReadFailureNonFatal asserts a Fleet API read error
// does not persist any status and does not panic (last-known preserved).
func TestServiceStatusMonitor_ReadFailureNonFatal(t *testing.T) {
	reader := &fakeVehicleReader{err: errors.New("tesla 500")}
	updater := &fakeStatusUpdater{}
	m := newTestMonitor(reader, updater)

	m.handleConnectivity(context.Background(), connectEvt())

	if got := reader.callCount(); got != 1 {
		t.Fatalf("reader calls = %d, want 1", got)
	}
	if got := updater.written(); len(got) != 0 {
		t.Fatalf("persisted statuses = %v, want none on read failure", got)
	}
}

// TestServiceStatusMonitor_InServiceTrueWritesStatus /
// _InServiceFalseLeavesStatus covers the write decision on the REST flag.
func TestServiceStatusMonitor_InServiceFlag(t *testing.T) {
	tests := []struct {
		name        string
		inService   bool
		wantWritten []string
	}{
		{"in_service true persists in_service", true, []string{"in_service"}},
		{"in_service false leaves last-known", false, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeVehicleReader{state: FleetVehicleState{State: "online", InService: tt.inService}}
			updater := &fakeStatusUpdater{}
			m := newTestMonitor(reader, updater)

			m.handleConnectivity(context.Background(), connectEvt())

			got := updater.written()
			if len(got) != len(tt.wantWritten) {
				t.Fatalf("persisted statuses = %v, want %v", got, tt.wantWritten)
			}
			for i := range got {
				if got[i] != tt.wantWritten[i] {
					t.Fatalf("persisted[%d] = %q, want %q", i, got[i], tt.wantWritten[i])
				}
			}
		})
	}
}

// TestServiceStatusMonitor_NoTokenSkipsRead asserts a missing owner token
// short-circuits before any Fleet API read (non-fatal).
func TestServiceStatusMonitor_NoTokenSkipsRead(t *testing.T) {
	reader := &fakeVehicleReader{state: FleetVehicleState{InService: true}}
	updater := &fakeStatusUpdater{}
	m := NewServiceStatusMonitor(
		nil, reader,
		&fakeTokenResolver{err: ErrTeslaTokenUnavailable},
		&stubVehicleOwner{ownerID: "user-1"},
		updater, nil,
	)

	m.handleConnectivity(context.Background(), connectEvt())

	if got := reader.callCount(); got != 0 {
		t.Fatalf("reader calls = %d, want 0 (no token)", got)
	}
	if got := updater.written(); len(got) != 0 {
		t.Fatalf("persisted statuses = %v, want none", got)
	}
}

// TestServiceStatusMonitor_UnknownOwnerSkipsRead asserts an owner-lookup
// failure short-circuits before any Fleet API read (non-fatal).
func TestServiceStatusMonitor_UnknownOwnerSkipsRead(t *testing.T) {
	reader := &fakeVehicleReader{state: FleetVehicleState{InService: true}}
	updater := &fakeStatusUpdater{}
	m := NewServiceStatusMonitor(
		nil, reader,
		&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
		&stubVehicleOwner{err: errors.New("not found")},
		updater, nil,
	)

	m.handleConnectivity(context.Background(), connectEvt())

	if got := reader.callCount(); got != 0 {
		t.Fatalf("reader calls = %d, want 0 (unknown owner)", got)
	}
}

// TestServiceStatusMonitor_StartStop exercises the bus subscription lifecycle
// end-to-end: a published ConnectivityEvent drives exactly one read.
func TestServiceStatusMonitor_StartStop(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	reader := &fakeVehicleReader{state: FleetVehicleState{State: "online", InService: true}}
	updater := &fakeStatusUpdater{}
	m := NewServiceStatusMonitor(
		bus, reader,
		&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}},
		&stubVehicleOwner{ownerID: "user-1"},
		updater, nil,
	)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	if err := bus.Publish(context.Background(), events.NewEvent(connectEvt())); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Poll for the async, serially-delivered read to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reader.callCount() == 1 && len(updater.written()) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("edge did not drive a read: calls=%d written=%v", reader.callCount(), updater.written())
}
