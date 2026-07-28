package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// --- fakes ---------------------------------------------------------------

// fakeReleaseNotesReader scripts GetReleaseNotes. It embeds *fakeVehicleReader
// so ONE double satisfies both the FleetVehicleReader the monitor requires and
// the optional ReleaseNotesReader — the same composition the MYR-316
// fakeServiceDataReader uses.
type fakeReleaseNotesReader struct {
	*fakeVehicleReader

	mu    sync.Mutex
	calls int
	notes []ReleaseNote
	err   error
}

func (f *fakeReleaseNotesReader) GetReleaseNotes(_ context.Context, _, _ string) ([]ReleaseNote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.notes, nil
}

func (f *fakeReleaseNotesReader) callCountReleaseNotes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeColorWriter records every colour write so a test can assert both WHAT was
// written and that nothing was written at all in the skip cases.
type fakeColorWriter struct {
	mu      sync.Mutex
	calls   int
	users   []string
	vins    []string
	colors  []string
	updated bool
	err     error
}

func (f *fakeColorWriter) UpdateVehicleColor(_ context.Context, userID, vin, color string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.users = append(f.users, userID)
	f.vins = append(f.vins, vin)
	f.colors = append(f.colors, color)
	if f.err != nil {
		return false, f.err
	}
	return f.updated, nil
}

func (f *fakeColorWriter) snapshot() (calls int, lastUser, lastVIN, lastColor string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == 0 {
		return 0, "", "", ""
	}
	return f.calls, f.users[len(f.users)-1], f.vins[len(f.vins)-1], f.colors[len(f.colors)-1]
}

// --- fsdVersion ----------------------------------------------------------

// TestAddFSDVersionField pins the ONE rule that governs every outcome: the
// field is added when — and only when — Tesla gave us a non-empty newest title.
// Every other case is the SAME no-op, because "we could not read it" must never
// be recorded as "this car has no FSD version".
func TestAddFSDVersionField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		notes     []ReleaseNote
		readErr   error
		unwired   bool
		emptyTok  bool
		wantCalls int
		wantValue string // "" means the field must be ABSENT
	}{
		{
			name:      "newest title is added verbatim",
			notes:     []ReleaseNote{{Title: ptr("FSD (Supervised) v14.3.5")}, {Title: ptr("FSD (Supervised) v14.2.0")}},
			wantCalls: 1,
			wantValue: "FSD (Supervised) v14.3.5",
		},
		{
			name:      "empty list adds nothing",
			notes:     []ReleaseNote{},
			wantCalls: 1,
		},
		{
			name:      "titleless newest entry adds nothing",
			notes:     []ReleaseNote{{Subtitle: ptr("no title")}},
			wantCalls: 1,
		},
		{
			name:      "read error adds nothing and is non-fatal",
			readErr:   errors.New("408 vehicle unavailable"),
			wantCalls: 1,
		},
		{
			name:    "option not wired: no call at all",
			unwired: true,
		},
		{
			name:     "no owner token: no call at all",
			notes:    []ReleaseNote{{Title: ptr("FSD (Supervised) v14.3.5")}},
			emptyTok: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := &fakeReleaseNotesReader{
				fakeVehicleReader: &fakeVehicleReader{},
				notes:             tt.notes,
				err:               tt.readErr,
			}
			opts := []ServiceStatusMonitorOption{}
			if !tt.unwired {
				opts = append(opts, WithReleaseNotes(reader))
			}
			m := newTestMonitor(reader.fakeVehicleReader, &fakeStatusUpdater{}, opts...)

			token := "tok"
			if tt.emptyTok {
				token = ""
			}
			fields := make(map[string]events.TelemetryValue)
			m.addFSDVersionField(context.Background(), svcTestVIN, token, fields)

			if got := reader.callCountReleaseNotes(); got != tt.wantCalls {
				t.Errorf("release_notes calls = %d, want %d", got, tt.wantCalls)
			}
			assertTelemetryString(t, fields, string(FieldFSDVersion), tt.wantValue)
		})
	}
}

// TestRefreshFromVehicleDataAddsFSDVersion proves the extra GET is hung off the
// EXISTING backfill — one vehicle_data read, one release_notes read, both from
// the same trigger — and that the resulting field reaches the published frame.
func TestRefreshFromVehicleDataAddsFSDVersion(t *testing.T) {
	t.Parallel()

	reader := &fakeReleaseNotesReader{
		fakeVehicleReader: &fakeVehicleReader{
			data: &VehicleData{
				VehicleConfig: &VehicleDataVehicleConfig{
					TrimBadging:        ptr("p74d"),
					PerformancePackage: ptr("Performance"),
				},
			},
		},
		notes: []ReleaseNote{{Title: ptr("FSD (Supervised) v14.3.5")}},
	}
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	got := collectTelemetry(t, bus)
	m := newBusMonitor(bus, reader.fakeVehicleReader, WithReleaseNotes(reader))

	if err := m.RefreshFromVehicleData(context.Background(), svcTestVIN, "tok"); err != nil {
		t.Fatalf("RefreshFromVehicleData: %v", err)
	}

	if got := reader.dataCallCount(); got != 1 {
		t.Errorf("vehicle_data calls = %d, want exactly 1", got)
	}
	if got := reader.callCountReleaseNotes(); got != 1 {
		t.Errorf("release_notes calls = %d, want exactly 1 (ONE additional GET)", got)
	}

	evt, ok := awaitBackfill(t, got)
	if !ok {
		t.Fatal("no backfill frame published")
	}
	fields := evt.Fields
	assertTelemetryString(t, fields, string(FieldTrim), "p74d")
	assertTelemetryString(t, fields, string(FieldTrimLabel), "Performance")
	assertTelemetryString(t, fields, string(FieldFSDVersion), "FSD (Supervised) v14.3.5")
}

// A failing release_notes read must not fail the backfill it rides along with:
// the vehicle_data fields still publish, minus fsdVersion.
func TestRefreshFromVehicleDataSurvivesReleaseNotesFailure(t *testing.T) {
	t.Parallel()

	reader := &fakeReleaseNotesReader{
		fakeVehicleReader: &fakeVehicleReader{
			data: &VehicleData{
				VehicleConfig: &VehicleDataVehicleConfig{PerformancePackage: ptr("Performance")},
			},
		},
		err: errors.New("408 vehicle unavailable"),
	}
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	got := collectTelemetry(t, bus)
	m := newBusMonitor(bus, reader.fakeVehicleReader, WithReleaseNotes(reader))

	if err := m.RefreshFromVehicleData(context.Background(), svcTestVIN, "tok"); err != nil {
		t.Fatalf("RefreshFromVehicleData must stay non-fatal on a release_notes failure: %v", err)
	}
	evt, ok := awaitBackfill(t, got)
	if !ok {
		t.Fatal("no backfill frame published")
	}
	fields := evt.Fields
	assertTelemetryString(t, fields, string(FieldTrimLabel), "Performance")
	assertTelemetryString(t, fields, string(FieldFSDVersion), "")
}

// --- colour --------------------------------------------------------------

// TestSyncVehicleColor pins the write rules. The one that matters most is the
// EMPTY SKIP: Tesla omitting exterior_color on a partial payload must never
// blank a colour an earlier read got right.
func TestSyncVehicleColor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		config    *VehicleDataVehicleConfig
		unwired   bool
		writeErr  error
		wantCalls int
		wantColor string
	}{
		{
			name:      "non-empty colour is written",
			config:    &VehicleDataVehicleConfig{ExteriorColor: ptr("Quicksilver")},
			wantCalls: 1,
			wantColor: "Quicksilver",
		},
		{
			name:   "EMPTY colour is never written",
			config: &VehicleDataVehicleConfig{ExteriorColor: ptr("")},
		},
		{
			name:   "absent colour is never written",
			config: &VehicleDataVehicleConfig{TrimBadging: ptr("p74d")},
		},
		{
			name: "absent vehicle_config is never written",
		},
		{
			name:    "option not wired: no call at all",
			config:  &VehicleDataVehicleConfig{ExteriorColor: ptr("Quicksilver")},
			unwired: true,
		},
		{
			name:      "write error is non-fatal",
			config:    &VehicleDataVehicleConfig{ExteriorColor: ptr("Quicksilver")},
			writeErr:  errors.New("connection reset"),
			wantCalls: 1,
			wantColor: "Quicksilver",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			writer := &fakeColorWriter{updated: true, err: tt.writeErr}
			opts := []ServiceStatusMonitorOption{}
			if !tt.unwired {
				opts = append(opts, WithVehicleColor(writer))
			}
			m := newTestMonitor(&fakeVehicleReader{}, &fakeStatusUpdater{}, opts...)

			m.syncVehicleColor(context.Background(), svcTestVIN, tt.config)

			calls, gotUser, gotVIN, gotColor := writer.snapshot()
			if calls != tt.wantCalls {
				t.Fatalf("colour writes = %d, want %d", calls, tt.wantCalls)
			}
			if tt.wantCalls == 0 {
				return
			}
			if gotColor != tt.wantColor {
				t.Errorf("colour = %q, want %q", gotColor, tt.wantColor)
			}
			// The write is owner-scoped: the resolved owner and the VIN are
			// both handed to the store so the WHERE clause can fail closed.
			if gotUser != "user-1" {
				t.Errorf("userID = %q, want %q", gotUser, "user-1")
			}
			if gotVIN != svcTestVIN {
				t.Errorf("vin = %q, want %q", gotVIN, svcTestVIN)
			}
		})
	}
}

// The colour write must ride the SAME vehicle_data read as everything else —
// no second Tesla call — and must happen even when the payload carries nothing
// else, which is the case the early empty-frame return would otherwise skip.
func TestRefreshFromVehicleDataWritesColorOnly(t *testing.T) {
	t.Parallel()

	reader := &fakeVehicleReader{
		data: &VehicleData{
			VehicleConfig: &VehicleDataVehicleConfig{ExteriorColor: ptr("Quicksilver")},
		},
	}
	writer := &fakeColorWriter{updated: true}
	m := newTestMonitor(reader, &fakeStatusUpdater{}, WithVehicleColor(writer))

	if err := m.RefreshFromVehicleData(context.Background(), svcTestVIN, "tok"); err != nil {
		t.Fatalf("RefreshFromVehicleData: %v", err)
	}

	if got := reader.dataCallCount(); got != 1 {
		t.Errorf("vehicle_data calls = %d, want exactly 1", got)
	}
	calls, _, _, gotColor := writer.snapshot()
	if calls != 1 || gotColor != "Quicksilver" {
		t.Errorf("colour write = (%d calls, %q), want (1, %q)", calls, gotColor, "Quicksilver")
	}
}

// A nil vehicle_data payload (Tesla can answer 200 with an empty body) must be
// a no-op, not a crash.
func TestRefreshFromVehicleDataNilPayloadIsSafe(t *testing.T) {
	t.Parallel()

	reader := &fakeVehicleReader{data: nil}
	writer := &fakeColorWriter{updated: true}
	m := newTestMonitor(reader, &fakeStatusUpdater{}, WithVehicleColor(writer))

	if err := m.RefreshFromVehicleData(context.Background(), svcTestVIN, "tok"); err != nil {
		t.Fatalf("RefreshFromVehicleData: %v", err)
	}
	if calls, _, _, _ := writer.snapshot(); calls != 0 {
		t.Errorf("colour writes = %d, want 0 for a nil payload", calls)
	}
}
