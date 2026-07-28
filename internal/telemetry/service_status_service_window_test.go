package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// fakeServiceDataReader scripts GetServiceData. It embeds *fakeVehicleReader so
// one double satisfies both the FleetVehicleReader the monitor already needs
// and the new ServiceDataReader.
type fakeServiceDataReader struct {
	*fakeVehicleReader

	mu    sync.Mutex
	calls int
	data  *ServiceData
	err   error
}

func (f *fakeServiceDataReader) GetServiceData(_ context.Context, _, _ string) (*ServiceData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.data, nil
}

func (f *fakeServiceDataReader) callCountServiceData() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeServiceWindowWriter records every write so a test can assert BOTH what
// was written and that a nil really reached the store as a clear.
type fakeServiceWindowWriter struct {
	mu sync.Mutex

	setCalls   int
	lastETC    *time.Time
	lastETCSet bool

	clearCalls int
	clearedFor []string
	clearErr   error
	setErr     error
}

func (f *fakeServiceWindowWriter) SetServiceETC(_ context.Context, _ string, etc *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	f.lastETC, f.lastETCSet = etc, true
	return f.setErr
}

func (f *fakeServiceWindowWriter) ClearServiceWindow(_ context.Context, vehicleID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearCalls++
	f.clearedFor = append(f.clearedFor, vehicleID)
	if f.clearErr != nil {
		return false, f.clearErr
	}
	return true, nil
}

func (f *fakeServiceWindowWriter) snapshot() (setCalls, clearCalls int, etc *time.Time, etcSet bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setCalls, f.clearCalls, f.lastETC, f.lastETCSet
}

// fakeVehicleIDLookup maps every VIN to one pinned vehicle id.
type fakeVehicleIDLookup struct {
	id  string
	err error
}

func (f *fakeVehicleIDLookup) GetVehicleID(context.Context, string) (string, error) {
	return f.id, f.err
}

const swTestVehicleID = "veh_service_window_001"

// newServiceWindowMonitor builds a monitor with the MYR-316 pipeline wired,
// over the same fakes the MYR-259/260 tests use. No live Tesla call can fire.
func newServiceWindowMonitor(
	reader *fakeServiceDataReader,
	writer *fakeServiceWindowWriter,
	ids *fakeVehicleIDLookup,
	opts ...ServiceStatusMonitorOption,
) *ServiceStatusMonitor {
	all := append([]ServiceStatusMonitorOption{
		WithServiceWindow(reader, writer, ids),
	}, opts...)
	return NewServiceStatusMonitor(
		nil, // no bus: these tests drive the handlers directly
		reader,
		&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}},
		&stubVehicleOwner{ownerID: "user-1"},
		&fakeStatusUpdater{},
		nil,
		all...,
	)
}

func newSWReader(state FleetVehicleState, data *ServiceData) *fakeServiceDataReader {
	return &fakeServiceDataReader{
		fakeVehicleReader: &fakeVehicleReader{state: state},
		data:              data,
	}
}

// An in-service car's connectivity edge reads service_data and stores Tesla's
// estimate — the core MYR-316 acquisition path.
func TestServiceWindow_InServiceEdgeStoresTeslaEstimate(t *testing.T) {
	etc := time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)
	reader := newSWReader(
		FleetVehicleState{State: "online", InService: true},
		&ServiceData{ServiceETC: &etc},
	)
	writer := &fakeServiceWindowWriter{}
	m := newServiceWindowMonitor(reader, writer, &fakeVehicleIDLookup{id: swTestVehicleID})

	m.handleConnectivity(context.Background(), connectEvt())

	if got := reader.callCountServiceData(); got != 1 {
		t.Fatalf("service_data calls = %d, want 1", got)
	}
	setCalls, clearCalls, gotETC, _ := writer.snapshot()
	if setCalls != 1 {
		t.Fatalf("SetServiceETC calls = %d, want 1", setCalls)
	}
	if clearCalls != 0 {
		t.Fatalf("ClearServiceWindow calls = %d, want 0 — the car IS in service", clearCalls)
	}
	if gotETC == nil || !gotETC.Equal(etc) {
		t.Fatalf("stored etc = %v, want %v", gotETC, etc)
	}
}

// An all-null service_data body must still WRITE — storing nil retracts a stale
// estimate. Skipping the write would leave yesterday's completion time on a
// visit Tesla no longer has a record for.
func TestServiceWindow_AllNullServiceDataStoresNil(t *testing.T) {
	reader := newSWReader(
		FleetVehicleState{State: "online", InService: true},
		&ServiceData{}, // every field nil — the verified-live common case
	)
	writer := &fakeServiceWindowWriter{}
	m := newServiceWindowMonitor(reader, writer, &fakeVehicleIDLookup{id: swTestVehicleID})

	m.handleConnectivity(context.Background(), connectEvt())

	setCalls, _, gotETC, etcSet := writer.snapshot()
	if setCalls != 1 {
		t.Fatalf("SetServiceETC calls = %d, want 1 — an absent estimate must still be written", setCalls)
	}
	if !etcSet {
		t.Fatal("SetServiceETC was never reached")
	}
	if gotETC != nil {
		t.Fatalf("stored etc = %v, want nil", gotETC)
	}
}

// A car that is NOT in service gets both columns cleared on the same edge —
// the auto-clear half of the contract.
func TestServiceWindow_NotInServiceEdgeClears(t *testing.T) {
	reader := newSWReader(FleetVehicleState{State: "online", InService: false}, nil)
	writer := &fakeServiceWindowWriter{}
	m := newServiceWindowMonitor(reader, writer, &fakeVehicleIDLookup{id: swTestVehicleID})

	m.handleConnectivity(context.Background(), connectEvt())

	if got := reader.callCountServiceData(); got != 0 {
		t.Fatalf("service_data calls = %d, want 0 — a car out of service has no window to read", got)
	}
	setCalls, clearCalls, _, _ := writer.snapshot()
	if setCalls != 0 {
		t.Fatalf("SetServiceETC calls = %d, want 0", setCalls)
	}
	if clearCalls != 1 {
		t.Fatalf("ClearServiceWindow calls = %d, want 1", clearCalls)
	}
	if len(writer.clearedFor) != 1 || writer.clearedFor[0] != swTestVehicleID {
		t.Fatalf("cleared for %v, want [%s]", writer.clearedFor, swTestVehicleID)
	}
}

// The ServiceMode ON→OFF edge is the transition-out path for a car that never
// disconnects. It must clear the window even when the confirming REST read
// FAILS — we trust the live signal exactly as the status write does, and a car
// reported out of service must not keep advertising an "expected back".
func TestServiceWindow_ServiceModeOffClearsEvenWhenReadFails(t *testing.T) {
	reader := newSWReader(FleetVehicleState{}, nil)
	reader.fakeVehicleReader.err = errors.New("fleet unavailable")
	writer := &fakeServiceWindowWriter{}
	m := newServiceWindowMonitor(reader, writer, &fakeVehicleIDLookup{id: swTestVehicleID})

	m.reconcileServiceModeOff(context.Background(), svcTestVIN)

	_, clearCalls, _, _ := writer.snapshot()
	if clearCalls != 1 {
		t.Fatalf("ClearServiceWindow calls = %d, want 1 even on a failed read", clearCalls)
	}
}

// A live ServiceMode OFF→ON edge is the car ENTERING service while streaming —
// no connectivity edge announces it, so the telemetry path must acquire the
// window itself.
func TestServiceWindow_ServiceModeOnAcquiresWindow(t *testing.T) {
	etc := time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC)
	reader := newSWReader(
		FleetVehicleState{State: "online", InService: true},
		&ServiceData{ServiceETC: &etc},
	)
	writer := &fakeServiceWindowWriter{}
	m := newServiceWindowMonitor(reader, writer, &fakeVehicleIDLookup{id: swTestVehicleID})

	on := true
	m.handleTelemetry(context.Background(), events.VehicleTelemetryEvent{
		VIN:    svcTestVIN,
		Fields: map[string]events.TelemetryValue{serviceModeFieldKey: {BoolVal: &on}},
	})

	setCalls, _, gotETC, _ := writer.snapshot()
	if setCalls != 1 {
		t.Fatalf("SetServiceETC calls = %d, want 1", setCalls)
	}
	if gotETC == nil || !gotETC.Equal(etc) {
		t.Fatalf("stored etc = %v, want %v", gotETC, etc)
	}
}

// The service_data read shares the existing per-VIN debounce, so a flapping
// connection cannot turn it into a Tesla poll.
func TestServiceWindow_ReadIsDebouncedPerVIN(t *testing.T) {
	etc := time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)
	reader := newSWReader(
		FleetVehicleState{State: "online", InService: true},
		&ServiceData{ServiceETC: &etc},
	)
	writer := &fakeServiceWindowWriter{}
	now := time.Now()
	m := newServiceWindowMonitor(reader, writer,
		&fakeVehicleIDLookup{id: swTestVehicleID},
		withServiceClock(func() time.Time { return now }), // frozen: the cooldown never lapses
	)

	for range 5 {
		m.handleConnectivity(context.Background(), connectEvt())
	}

	if got := reader.callCountServiceData(); got != 1 {
		t.Fatalf("service_data calls = %d across 5 edges, want 1 (debounced)", got)
	}
}

// Every service-window failure is non-fatal: a read error leaves the last-known
// estimate, and a write error is logged rather than propagated. Neither may
// disturb the status write the sync rides along with.
func TestServiceWindow_FailuresAreNonFatal(t *testing.T) {
	t.Run("read failure leaves the estimate alone", func(t *testing.T) {
		reader := newSWReader(FleetVehicleState{State: "online", InService: true}, nil)
		reader.err = errors.New("service_data 500")
		writer := &fakeServiceWindowWriter{}
		m := newServiceWindowMonitor(reader, writer, &fakeVehicleIDLookup{id: swTestVehicleID})

		m.handleConnectivity(context.Background(), connectEvt())

		setCalls, clearCalls, _, _ := writer.snapshot()
		if setCalls != 0 {
			t.Errorf("SetServiceETC calls = %d, want 0 — a failed read must not overwrite", setCalls)
		}
		if clearCalls != 0 {
			t.Errorf("ClearServiceWindow calls = %d, want 0 — a failed read is not 'out of service'", clearCalls)
		}
	})

	t.Run("write failure does not panic or propagate", func(t *testing.T) {
		etc := time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)
		reader := newSWReader(
			FleetVehicleState{State: "online", InService: true},
			&ServiceData{ServiceETC: &etc},
		)
		writer := &fakeServiceWindowWriter{setErr: errors.New("db down")}
		m := newServiceWindowMonitor(reader, writer, &fakeVehicleIDLookup{id: swTestVehicleID})

		m.handleConnectivity(context.Background(), connectEvt())

		setCalls, _, _, _ := writer.snapshot()
		if setCalls != 1 {
			t.Errorf("SetServiceETC calls = %d, want 1", setCalls)
		}
	})

	t.Run("vehicle id lookup failure skips the sync entirely", func(t *testing.T) {
		reader := newSWReader(FleetVehicleState{State: "online", InService: true}, &ServiceData{})
		writer := &fakeServiceWindowWriter{}
		m := newServiceWindowMonitor(reader, writer, &fakeVehicleIDLookup{err: errors.New("no such VIN")})

		m.handleConnectivity(context.Background(), connectEvt())

		setCalls, clearCalls, _, _ := writer.snapshot()
		if setCalls != 0 || clearCalls != 0 {
			t.Errorf("writes attempted without a vehicle id: set=%d clear=%d", setCalls, clearCalls)
		}
	})
}

// Without the option the monitor behaves exactly as it did before MYR-316 —
// the property every pre-existing monitor test depends on.
func TestServiceWindow_DisabledWhenOptionOmitted(t *testing.T) {
	reader := newSWReader(FleetVehicleState{State: "online", InService: true}, &ServiceData{})
	m := NewServiceStatusMonitor(
		nil,
		reader,
		&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}},
		&stubVehicleOwner{ownerID: "user-1"},
		&fakeStatusUpdater{},
		nil,
	)

	m.handleConnectivity(context.Background(), connectEvt())

	if got := reader.callCountServiceData(); got != 0 {
		t.Fatalf("service_data calls = %d, want 0 when the option is not wired", got)
	}
}
