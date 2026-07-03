package drives

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// fakeOpenDriveLister is a minimal in-memory OpenDriveLister used by
// the reconciliation tests. rows is returned verbatim from ListOpen;
// err (if set) is returned instead.
//
// endedIDs records every drive ID the reconciler asked us to close
// (in call order); when markEndedErr is non-nil it is returned to the
// reconciler instead, exercising the fail-open path.
type fakeOpenDriveLister struct {
	mu sync.Mutex

	rows []OpenDriveRow
	err  error

	endedIDs     []string
	markEndedErr error
}

func (f *fakeOpenDriveLister) ListOpen(_ context.Context) ([]OpenDriveRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *fakeOpenDriveLister) MarkOrphanEnded(_ context.Context, driveID string) error {
	if f.markEndedErr != nil {
		return f.markEndedErr
	}
	f.mu.Lock()
	f.endedIDs = append(f.endedIDs, driveID)
	f.mu.Unlock()
	return nil
}

// endedSnapshot returns a copy of the captured drive IDs in call order,
// safe to inspect without holding f.mu.
func (f *fakeOpenDriveLister) endedSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.endedIDs))
	copy(out, f.endedIDs)
	return out
}

// loadStateForTest is a test-only helper that pulls a vehicleState out
// of the Detector's internal map. Returns nil when no entry exists.
func (d *Detector) loadStateForTest(vin string) *vehicleState {
	val, ok := d.states.Load(vin)
	if !ok {
		return nil
	}
	return val.(*vehicleState)
}

// TestDetector_EndsAllOrphansOnStart is the core MYR-149 (Option A)
// behavior: every open Drive row found on Start is closed
// synchronously via MarkOrphanEnded. No in-memory state is created for
// any of them — the reattach path was removed because it trapped
// drives forever when the car streamed idle telemetry.
func TestDetector_EndsAllOrphansOnStart(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	startTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	lister := &fakeOpenDriveLister{
		rows: []OpenDriveRow{
			{ID: "drv_recon_1", VIN: "V1", StartTime: startTime, StartChargeLevel: 80},
			{ID: "drv_recon_2", VIN: "V2", StartTime: startTime, StartChargeLevel: 65},
		},
	}

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, lister)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	ended := lister.endedSnapshot()
	if len(ended) != 2 {
		t.Fatalf("MarkOrphanEnded calls = %d (%v), want 2", len(ended), ended)
	}
	endedSet := map[string]bool{ended[0]: true, ended[1]: true}
	if !endedSet["drv_recon_1"] || !endedSet["drv_recon_2"] {
		t.Errorf("MarkOrphanEnded called for %v, want drv_recon_1 and drv_recon_2", ended)
	}

	for _, vin := range []string{"V1", "V2"} {
		if state := d.loadStateForTest(vin); state != nil {
			t.Errorf("VIN %s has in-memory state after reconciliation; the reattach path must be gone", vin)
		}
	}
	if got := d.activeCount.Load(); got != 0 {
		t.Errorf("activeCount = %d, want 0 (orphans are closed, not reattached)", got)
	}
}

// TestDetector_MultipleOrphansSameVIN_AllEnded: MYR-147 found 3
// simultaneously-open rows for one VIN. Since MYR-149 ALL of them are
// closed synchronously — including the most recent, which previously
// went through the in-memory reattach path and got stuck.
func TestDetector_MultipleOrphansSameVIN_AllEnded(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	const vin = "V1"
	now := time.Now().UTC()
	t1 := now.Add(-30 * time.Minute).Format(time.RFC3339)
	t2 := now.Add(-20 * time.Minute).Format(time.RFC3339)
	t3 := now.Add(-10 * time.Minute).Format(time.RFC3339)

	lister := &fakeOpenDriveLister{
		rows: []OpenDriveRow{
			{ID: "drv_t2_middle", VIN: vin, StartTime: t2, StartChargeLevel: 70},
			{ID: "drv_t3_latest", VIN: vin, StartTime: t3, StartChargeLevel: 65},
			{ID: "drv_t1_oldest", VIN: vin, StartTime: t1, StartChargeLevel: 80},
		},
	}

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, lister)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	ended := lister.endedSnapshot()
	if len(ended) != 3 {
		t.Fatalf("MarkOrphanEnded calls = %d (%v), want 3 (every orphan, including the latest)", len(ended), ended)
	}
	if state := d.loadStateForTest(vin); state != nil {
		t.Errorf("VIN %s has in-memory state after reconciliation, want none", vin)
	}
	if got := d.activeCount.Load(); got != 0 {
		t.Errorf("activeCount = %d, want 0", got)
	}
}

// TestDetector_Orphan_IdleTelemetryAfterStart_DoesNotTrapDrive is the
// MYR-149 acceptance test. Live failure mode being reproduced: the car
// is parked but online, streaming idle telemetry pings. Under the old
// reattach design each ping (1) refreshed lastTelemetryAt so the
// silence watchdog never fired and (2) cleared the reconciled bypass
// so an eventual end was discarded by the micro-drive filter — the
// orphan row stayed open forever. With Option A the orphan is closed
// synchronously at Start and idle pings must not resurrect it.
func TestDetector_Orphan_IdleTelemetryAfterStart_DoesNotTrapDrive(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	const orphanID = "drv_trapped"
	const vin = "VIN_IDLE_STREAM"

	startTime := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	lister := &fakeOpenDriveLister{
		rows: []OpenDriveRow{
			{ID: orphanID, VIN: vin, StartTime: startTime, StartChargeLevel: 70},
		},
	}

	// Prod-realistic thresholds: under the old design these are what
	// made the trap permanent (micro-drive filter discarded the end).
	cfg := testConfig()
	cfg.EndDebounce = 200 * time.Millisecond
	cfg.MinDuration = 2 * time.Minute
	cfg.MinDistanceMiles = 0.1

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, lister)

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// The orphan must be closed synchronously at Start — the "new
	// path", not the watchdog-relies-on-silence path.
	ended := lister.endedSnapshot()
	if len(ended) != 1 || ended[0] != orphanID {
		t.Fatalf("MarkOrphanEnded calls = %v, want exactly [%s] at Start", ended, orphanID)
	}

	// The car keeps streaming idle telemetry (parked but online): SOC
	// pings with gear=P. These must not attach to the closed orphan or
	// start a new drive.
	for i := 0; i < 5; i++ {
		fields := gearField("P")
		fields[string(telemetry.FieldSOC)] = events.TelemetryValue{FloatVal: ptr(75.5)}
		publishTelemetry(t, bus, telemetryEvent(vin, time.Now(), fields))
	}

	expectNoEvent(t, startedCh, 300*time.Millisecond,
		"idle pings after reconciliation must not start a drive")

	if state := d.loadStateForTest(vin); state != nil {
		state.mu.Lock()
		status := state.status
		state.mu.Unlock()
		if status != StatusIdle {
			t.Errorf("VIN %s status = %v after idle pings, want StatusIdle", vin, status)
		}
	}

	// A real drive later starts FRESH — it must not reuse the closed
	// orphan's ID.
	publishTelemetry(t, bus, telemetryEvent(vin, time.Now(), driveFields("D", 35.0, 33.10, -96.83)))
	evt, ok := waitForEvent(startedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveStartedEvent for the fresh drive")
	}
	started, ok := evt.Payload.(events.DriveStartedEvent)
	if !ok {
		t.Fatalf("expected DriveStartedEvent, got %T", evt.Payload)
	}
	if started.DriveID == orphanID {
		t.Errorf("fresh drive reused the closed orphan ID %q; want a new drive ID", orphanID)
	}
}

func TestDetector_ReconcilerError_DoesNotFailStart(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	lister := &fakeOpenDriveLister{
		err: errors.New("simulated DB outage"),
	}

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, lister)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start must not propagate reconciler errors; got %v", err)
	}
	defer func() { _ = d.Stop() }()

	// No state should have been populated.
	if got := d.loadStateForTest("V1"); got != nil {
		t.Errorf("reconciler error should leave states empty; found state for V1: %+v", got)
	}
}

func TestDetector_NilReconciler_DoesNotPanic(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start with nil reconciler: %v", err)
	}
	defer func() { _ = d.Stop() }()
}

// TestDetector_MarkOrphanEndedError_DoesNotFailStart mirrors the
// fail-open posture of ListOpen: a DB hiccup on MarkOrphanEnded must
// not block Detector.Start. The row stays open in the DB and gets
// retried on the next restart.
func TestDetector_MarkOrphanEndedError_DoesNotFailStart(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	const vin = "V1"
	startTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)

	lister := &fakeOpenDriveLister{
		rows: []OpenDriveRow{
			{ID: "drv_stuck", VIN: vin, StartTime: startTime, StartChargeLevel: 50},
		},
		markEndedErr: errors.New("simulated DB outage on UPDATE Drive SET endTime"),
	}

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, lister)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start must not propagate MarkOrphanEnded errors; got %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Even on failure, no in-memory state is created — the reattach
	// path is gone regardless of side-channel success.
	if state := d.loadStateForTest(vin); state != nil {
		t.Errorf("MarkOrphanEnded failure must not create in-memory state; got %+v", state)
	}
	if got := d.activeCount.Load(); got != 0 {
		t.Errorf("activeCount = %d, want 0", got)
	}
}
