package drives

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// fakeOpenDriveLister is a minimal in-memory OpenDriveLister used by
// the reconciliation tests. rows is returned verbatim from ListOpen;
// err (if set) is returned instead.
//
// MYR-147: also captures MarkOrphanEnded calls. endedIDs records every
// drive ID the reconciler asked us to close (in call order); when
// markEndedErr is non-nil it is returned to the reconciler instead,
// exercising the fail-open path.
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

// loadState is a test-only helper that pulls a vehicleState out of the
// Detector's internal map. Returns nil when no entry exists.
func (d *Detector) loadStateForTest(vin string) *vehicleState {
	val, ok := d.states.Load(vin)
	if !ok {
		return nil
	}
	return val.(*vehicleState)
}

func TestDetector_ReconcilesOpenDrivesOnStart(t *testing.T) {
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

	for _, vin := range []string{"V1", "V2"} {
		state := d.loadStateForTest(vin)
		if state == nil {
			t.Fatalf("no reconciled state for VIN %s", vin)
		}
		state.mu.Lock()
		status := state.status
		var driveID string
		var startCharge float64
		if state.drive != nil {
			driveID = state.drive.id
			startCharge = state.drive.startCharge
		}
		lastTel := state.lastTelemetryAt
		state.mu.Unlock()

		if status != StatusDriving {
			t.Errorf("VIN %s status = %v, want StatusDriving", vin, status)
		}
		if driveID == "" {
			t.Errorf("VIN %s drive ID is empty, want non-empty", vin)
		}
		if lastTel.IsZero() {
			t.Errorf("VIN %s lastTelemetryAt is zero; watchdog needs a non-zero reference", vin)
		}
		if vin == "V1" && driveID != "drv_recon_1" {
			t.Errorf("VIN V1 drive ID = %q, want drv_recon_1", driveID)
		}
		if vin == "V1" && startCharge != 80 {
			t.Errorf("VIN V1 startCharge = %f, want 80", startCharge)
		}
	}
}

func TestDetector_ReconciledDrive_TelemetryResumes_KeepsSameDriveID(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	const reconciledID = "drv_resume_1"
	const vin = "VIN_RESUME"

	startTime := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	lister := &fakeOpenDriveLister{
		rows: []OpenDriveRow{
			{ID: reconciledID, VIN: vin, StartTime: startTime, StartChargeLevel: 70},
		},
	}

	cfg := testConfig()
	// Keep EndDebounce large so the watchdog cannot fire during the test.
	cfg.EndDebounce = 5 * time.Second
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, lister)

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	updatedCh := subscribeTopic(t, bus, events.TopicDriveUpdated)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Real telemetry resumes for the reconciled VIN. The detector
	// should NOT publish a new DriveStartedEvent — it should treat
	// this as continuation of the existing drive and publish a
	// DriveUpdatedEvent with the original drive ID.
	publishTelemetry(t, bus, telemetryEvent(vin, time.Now(), driveFields("D", 35.0, 33.1, -96.85)))

	evt, ok := waitForEvent(updatedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveUpdatedEvent after reconciliation")
	}
	upd, ok := evt.Payload.(events.DriveUpdatedEvent)
	if !ok {
		t.Fatalf("expected DriveUpdatedEvent, got %T", evt.Payload)
	}
	if upd.DriveID != reconciledID {
		t.Errorf("DriveID = %q, want %q (reconciled drive must continue, not restart)", upd.DriveID, reconciledID)
	}

	// No DriveStartedEvent should have been emitted — the reconciled
	// state is already StatusDriving, so handleTelemetry routes to
	// handleDriving, not handleIdle.
	expectNoEvent(t, startedCh, 200*time.Millisecond, "telemetry on reconciled drive must not emit a new DriveStartedEvent")
}

func TestDetector_ReconciledDrive_NoTelemetry_WatchdogEnds(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	const reconciledID = "drv_silent_1"
	const vin = "VIN_SILENT_RECON"

	startTime := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	lister := &fakeOpenDriveLister{
		rows: []OpenDriveRow{
			{ID: reconciledID, VIN: vin, StartTime: startTime, StartChargeLevel: 50},
		},
	}

	// Use prod-realistic micro-drive thresholds (MinDuration=2m,
	// MinDistanceMiles=0.1 — the actual defaults from
	// internal/config/defaults.go). A reconciled drive with no resumed
	// telemetry has Duration=0, Distance=0; without the reconciled
	// bypass in endDrive, the filter would discard the
	// DriveEndedEvent and the DB row would stay open forever
	// (MYR-146 critical regression). This test would have failed
	// before the bypass was added.
	cfg := testConfig()
	cfg.EndDebounce = 200 * time.Millisecond
	cfg.MinDuration = 2 * time.Minute
	cfg.MinDistanceMiles = 0.1

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, lister)

	// Injectable clock so we can advance past EndDebounce without
	// sleeping. Mirrors TestDetector_WatchdogEndsDriveWhenTelemetrySilent.
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

	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// No telemetry arrives for the reconciled VIN. Advance the wall
	// clock past EndDebounce; the next watchdog tick must observe the
	// silence and end the drive with the original drive ID.
	advanceClock(2 * cfg.EndDebounce)

	evt, ok := waitForEvent(endedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveEndedEvent from watchdog")
	}
	ended, ok := evt.Payload.(events.DriveEndedEvent)
	if !ok {
		t.Fatalf("expected DriveEndedEvent, got %T", evt.Payload)
	}
	if ended.DriveID != reconciledID {
		t.Errorf("DriveID = %q, want %q (watchdog must close the reconciled row)", ended.DriveID, reconciledID)
	}
	if ended.VIN != vin {
		t.Errorf("VIN = %q, want %q", ended.VIN, vin)
	}
}

// TestDetector_RegularMicroDrive_StillFiltered is the negative
// counterpart to TestDetector_ReconciledDrive_NoTelemetry_WatchdogEnds.
// It proves the micro-drive bypass in endDrive applies ONLY to
// reconciled drives — a regular drive that starts from a single D
// telemetry event and goes silent (no resumed telemetry, same
// "Duration=0, Distance=0" shape as the reconciled case) MUST still be
// discarded by the prod-default micro-drive filter. Otherwise the
// bypass would leak and accept every short blip as a real drive.
func TestDetector_RegularMicroDrive_StillFiltered(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	const vin = "VIN_REGULAR_MICRO"

	// Prod-realistic thresholds. No reconciler — this is a fresh,
	// non-reconciled drive.
	cfg := testConfig()
	cfg.EndDebounce = 200 * time.Millisecond
	cfg.MinDuration = 2 * time.Minute
	cfg.MinDistanceMiles = 0.1

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)

	// Injectable clock so we can advance past EndDebounce without
	// sleeping for the watchdog tick.
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

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Single D-gear telemetry event starts the drive. Critically, no
	// further telemetry arrives — so the watchdog-end path will see
	// Duration=0 (lastTimestamp == startedAt) and Distance=0 (a single
	// route point yields zero totalDistance), the same shape as a
	// reconciled drive with no resumed telemetry. The only difference
	// is the reconciled flag, which is false here.
	publishTelemetry(t, bus, telemetryEvent(vin, wallNow, driveFields("D", 5.0, 33.09, -96.82)))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent on regular drive")
	}

	// Advance past EndDebounce so the watchdog ends the drive.
	advanceClock(2 * cfg.EndDebounce)

	// With reconciled=false and prod-default micro-drive thresholds,
	// endDrive MUST discard the drive — no DriveEndedEvent.
	expectNoEvent(t, endedCh, 500*time.Millisecond,
		"regular micro-drive must still be filtered by endDrive — bypass is only for reconciled drives")
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

// TestDetector_MultipleOrphansSameVIN_KeepsLatestEndsOlder is the
// regression test for MYR-147. The MYR-146 reconciler keyed d.states
// by VIN with a Store-not-LoadOrStore, so when ListOpen returned
// multiple open Drive rows for the same VIN only the LAST one
// survived in memory; the rest were silently dropped and stayed open
// in the DB forever. Prod confirmed 3 such rows for the same VIN
// before the fix landed.
//
// After MYR-147: the reconciler groups by VIN, picks the most recent
// orphan (lexicographic StartTime, RFC3339-safe) for the in-memory
// reattach path, and synchronously closes every older row via
// MarkOrphanEnded. activeCount counts only the reattached drives —
// the side-channel end path doesn't go through the watchdog so it
// must not bump the counter.
func TestDetector_MultipleOrphansSameVIN_KeepsLatestEndsOlder(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	const vin = "V1"
	const (
		idT1 = "drv_t1_oldest"
		idT2 = "drv_t2_middle"
		idT3 = "drv_t3_latest"
	)

	now := time.Now().UTC()
	t1 := now.Add(-30 * time.Minute).Format(time.RFC3339)
	t2 := now.Add(-20 * time.Minute).Format(time.RFC3339)
	t3 := now.Add(-10 * time.Minute).Format(time.RFC3339)

	// Intentionally seed in a non-sorted order so the grouping logic
	// can't rely on input ordering.
	lister := &fakeOpenDriveLister{
		rows: []OpenDriveRow{
			{ID: idT2, VIN: vin, StartTime: t2, StartChargeLevel: 70},
			{ID: idT3, VIN: vin, StartTime: t3, StartChargeLevel: 65},
			{ID: idT1, VIN: vin, StartTime: t1, StartChargeLevel: 80},
		},
	}

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, lister)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// In-memory state must reflect the LATEST orphan only.
	state := d.loadStateForTest(vin)
	if state == nil {
		t.Fatalf("no reconciled state for VIN %s", vin)
	}
	state.mu.Lock()
	gotID := ""
	if state.drive != nil {
		gotID = state.drive.id
	}
	state.mu.Unlock()
	if gotID != idT3 {
		t.Errorf("reconciled drive ID = %q, want %q (latest by StartTime)", gotID, idT3)
	}

	// The two older drives must have been ended synchronously, in any
	// order.
	ended := lister.endedSnapshot()
	if len(ended) != 2 {
		t.Fatalf("MarkOrphanEnded calls = %d (%v), want 2 (%s and %s)", len(ended), ended, idT1, idT2)
	}
	endedSet := map[string]bool{ended[0]: true, ended[1]: true}
	if !endedSet[idT1] || !endedSet[idT2] {
		t.Errorf("MarkOrphanEnded called for %v, want both %s and %s", ended, idT1, idT2)
	}
	if endedSet[idT3] {
		t.Errorf("MarkOrphanEnded must NOT be called for the LATEST orphan %s", idT3)
	}

	// activeCount counts only the in-memory reattach (one per VIN).
	if got := d.activeCount.Load(); got != 1 {
		t.Errorf("activeCount = %d, want 1 (only the reattached drive)", got)
	}
}

// TestDetector_SingleOrphan_StillReattaches is the regression guard for
// the MYR-146 single-orphan path. With one open drive per VIN, the
// new grouping must behave identically: state populated,
// MarkOrphanEnded NOT called, activeCount == 1.
func TestDetector_SingleOrphan_StillReattaches(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	const (
		vin   = "V1"
		drvID = "drv_solo"
	)
	startTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	lister := &fakeOpenDriveLister{
		rows: []OpenDriveRow{
			{ID: drvID, VIN: vin, StartTime: startTime, StartChargeLevel: 75},
		},
	}

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, lister)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	state := d.loadStateForTest(vin)
	if state == nil {
		t.Fatalf("no reconciled state for VIN %s", vin)
	}
	state.mu.Lock()
	gotID := ""
	if state.drive != nil {
		gotID = state.drive.id
	}
	state.mu.Unlock()
	if gotID != drvID {
		t.Errorf("reconciled drive ID = %q, want %q", gotID, drvID)
	}

	if got := lister.endedSnapshot(); len(got) != 0 {
		t.Errorf("MarkOrphanEnded must not be called when there is no older same-VIN orphan; got %v", got)
	}
	if got := d.activeCount.Load(); got != 1 {
		t.Errorf("activeCount = %d, want 1", got)
	}
}

// TestDetector_OrphansAcrossDifferentVINs proves grouping doesn't
// break the cross-VIN case: three orphans, three different VINs →
// all three reattached, none ended synchronously, activeCount == 3.
func TestDetector_OrphansAcrossDifferentVINs(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	startTime := time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339)
	lister := &fakeOpenDriveLister{
		rows: []OpenDriveRow{
			{ID: "drv_v1", VIN: "V1", StartTime: startTime, StartChargeLevel: 80},
			{ID: "drv_v2", VIN: "V2", StartTime: startTime, StartChargeLevel: 60},
			{ID: "drv_v3", VIN: "V3", StartTime: startTime, StartChargeLevel: 40},
		},
	}

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, lister)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	want := map[string]string{
		"V1": "drv_v1",
		"V2": "drv_v2",
		"V3": "drv_v3",
	}
	for vin, wantID := range want {
		state := d.loadStateForTest(vin)
		if state == nil {
			t.Errorf("no reconciled state for VIN %s", vin)
			continue
		}
		state.mu.Lock()
		gotID := ""
		if state.drive != nil {
			gotID = state.drive.id
		}
		state.mu.Unlock()
		if gotID != wantID {
			t.Errorf("VIN %s drive ID = %q, want %q", vin, gotID, wantID)
		}
	}

	if got := lister.endedSnapshot(); len(got) != 0 {
		t.Errorf("MarkOrphanEnded must not be called across different VINs; got %v", got)
	}
	if got := d.activeCount.Load(); got != 3 {
		t.Errorf("activeCount = %d, want 3 (one per VIN)", got)
	}
}

// TestDetector_MarkOrphanEndedError_DoesNotFailStart mirrors the
// existing fail-open posture of ListOpen: a DB hiccup on the side
// channel must not block Detector.Start. The older orphan stays open
// in the DB and gets retried on the next restart.
func TestDetector_MarkOrphanEndedError_DoesNotFailStart(t *testing.T) {
	bus := testBus()
	defer bus.Close(context.Background())

	const vin = "V1"
	startTime := time.Now().UTC()
	t1 := startTime.Add(-30 * time.Minute).Format(time.RFC3339)
	t2 := startTime.Add(-10 * time.Minute).Format(time.RFC3339)

	lister := &fakeOpenDriveLister{
		rows: []OpenDriveRow{
			{ID: "drv_older", VIN: vin, StartTime: t1, StartChargeLevel: 50},
			{ID: "drv_latest", VIN: vin, StartTime: t2, StartChargeLevel: 45},
		},
		markEndedErr: errors.New("simulated DB outage on UPDATE Drive SET endTime"),
	}

	d := NewDetector(bus, testConfig(), testLogger(), NoopDetectorMetrics{}, lister)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start must not propagate MarkOrphanEnded errors; got %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Latest still reattached — the fail-open path is per-older-row,
	// not whole-reconciler.
	state := d.loadStateForTest(vin)
	if state == nil {
		t.Fatalf("latest orphan must still be reattached when MarkOrphanEnded fails")
	}
	state.mu.Lock()
	gotID := ""
	if state.drive != nil {
		gotID = state.drive.id
	}
	state.mu.Unlock()
	if gotID != "drv_latest" {
		t.Errorf("reconciled drive ID = %q, want %q", gotID, "drv_latest")
	}

	// activeCount still counts the reattach. We don't end-count the
	// failed call, so the metric reflects "row is still open" without
	// over-counting.
	if got := d.activeCount.Load(); got != 1 {
		t.Errorf("activeCount = %d, want 1 (reattach still counted on side-channel failure)", got)
	}
}

func TestParseDriveStartTime(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "RFC3339 with Z", in: "2026-06-06T20:33:06Z", wantErr: false},
		{name: "RFC3339 with offset", in: "2026-06-06T20:33:06+00:00", wantErr: false},
		{name: "RFC3339Nano", in: "2026-06-06T20:33:06.123456789Z", wantErr: false},
		{name: "no timezone (last-resort)", in: "2026-06-06T20:33:06", wantErr: false},
		{name: "empty string", in: "", wantErr: true},
		{name: "garbage", in: "not a time", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseDriveStartTime(tt.in)
			if tt.wantErr && err == nil {
				t.Errorf("parseDriveStartTime(%q): want error, got nil", tt.in)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("parseDriveStartTime(%q): unexpected error %v", tt.in, err)
			}
		})
	}
}
