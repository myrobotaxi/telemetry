package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
	"github.com/tnando/my-robo-taxi-telemetry/internal/mask"
)

// fakeAuditEmitter is a hub-level test AuditEmitter that records every
// call and exposes a snapshot for assertions. The struct mirrors the
// fakeAuditEmitter in internal/mask/audit_test.go but cannot be reused
// across packages — keeping a copy here avoids a cyclic test-only
// import.
type fakeAuditEmitter struct {
	mu      sync.Mutex
	entries []mask.AuditEntry
	err     error
}

func (f *fakeAuditEmitter) InsertAuditLog(_ context.Context, entry mask.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, entry)
	return f.err
}

func (f *fakeAuditEmitter) snapshot() []mask.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]mask.AuditEntry, len(f.entries))
	copy(out, f.entries)
	return out
}

// fakeAuditMetrics counts successful and failed audit writes.
type fakeAuditMetrics struct {
	writes   atomic.Int64
	failures atomic.Int64
}

func (f *fakeAuditMetrics) IncAuditWrite(string, string)        { f.writes.Add(1) }
func (f *fakeAuditMetrics) IncAuditWriteFailure(string, string) { f.failures.Add(1) }

// waitForEmitter polls until f.snapshot() reaches the desired length
// or the deadline expires. EmitAsync runs on a goroutine, so the test
// must wait for it to settle before asserting.
func waitForEmitter(t *testing.T, f *fakeAuditEmitter, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if len(f.snapshot()) >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d emitter entries, got %d", want, len(f.snapshot()))
		case <-tick.C:
		}
	}
}

// TestHub_BroadcastMasked_EmitsAudit_OnViewerStrip exercises the WS
// audit-emit gate end-to-end through BroadcastMasked. A viewer-role
// client subscribes to a vehicle whose mask strips licensePlate. The
// hub uses a per-vehicle frame counter starting at 1 and incrementing
// per BroadcastMasked call; the test loops until ShouldAuditWS samples
// in (1% rate, modulus 100), then asserts an audit row landed with the
// canonical metadata shape from rest-api.md §5.3.
func TestHub_BroadcastMasked_EmitsAudit_OnViewerStrip(t *testing.T) {
	emitter := &fakeAuditEmitter{}
	metrics := &fakeAuditMetrics{}

	hub := NewHub(slog.Default(), NoopHubMetrics{}, WithMaskAudit(emitter, metrics))
	t.Cleanup(hub.Stop)

	a := &testAuth{
		userID:        "user-1",
		vehicleIDs:    []string{"v-1"},
		roleByVehicle: map[string]auth.Role{"v-1": auth.RoleViewer},
	}
	srv := newTestServer(t, hub, a)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "valid-token")
	t.Cleanup(func() { _ = conn.CloseNow() })

	waitForClients(t, hub, 1)

	// Compute up front which frameSeq value will sample in for our
	// (vehicleID="v-1", role=viewer) pair. ShouldAuditWS uses
	// 1-indexed counters per nextFrameSeq, so loop from 1 upward.
	const vehicleID = "v-1"
	role := auth.RoleViewer
	var firstHit uint64 = 0
	for seq := uint64(1); seq <= 5000; seq++ {
		if mask.ShouldAuditWS(vehicleID, role, seq) {
			firstHit = seq
			break
		}
	}
	if firstHit == 0 {
		t.Fatal("no ShouldAuditWS hit found in 5000 frames; sampler is broken")
	}

	// Drive BroadcastMasked exactly firstHit times with a payload
	// that the viewer mask strips at least one field from
	// (licensePlate is owner-only).
	payload := map[string]any{
		"speed":        65,
		"licensePlate": "ABC-123",
	}
	for i := uint64(0); i < firstHit; i++ {
		hub.BroadcastMasked(
			vehicleID,
			mask.ResourceVehicleState,
			time.Now().UTC().Format(time.RFC3339),
			payload,
		)
	}

	waitForEmitter(t, emitter, 1)

	got := emitter.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 audit entry, got %d", len(got))
	}
	entry := got[0]
	if entry.Action != "mask_applied" {
		t.Errorf("Action = %q, want mask_applied", entry.Action)
	}
	if entry.TargetType != string(mask.TargetWSBroadcast) {
		t.Errorf("TargetType = %q, want ws_broadcast", entry.TargetType)
	}
	if entry.TargetID != vehicleID {
		t.Errorf("TargetID = %q, want %q", entry.TargetID, vehicleID)
	}
	if entry.UserID != "" {
		t.Errorf("UserID = %q; WS audit row must use empty userID (per-vehicle/role/frame, not per-client)", entry.UserID)
	}

	var meta map[string]any
	if err := json.Unmarshal(entry.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta["role"] != string(auth.RoleViewer) {
		t.Errorf("metadata.role = %v, want viewer", meta["role"])
	}
	if meta["channel"] != string(mask.AuditChannelWS) {
		t.Errorf("metadata.channel = %v, want ws", meta["channel"])
	}
	fields, ok := meta["fieldsMasked"].([]any)
	if !ok || len(fields) == 0 {
		t.Fatalf("metadata.fieldsMasked = %v, want non-empty list", meta["fieldsMasked"])
	}

	// Metric sanity: at least one successful write logged.
	if got := metrics.writes.Load(); got < 1 {
		t.Errorf("expected >=1 audit_log_writes_total, got %d", got)
	}
}

// TestHub_BroadcastMasked_NoAudit_OnOwnerNoStrip verifies the gate's
// "len(fieldsMasked) > 0" precondition: an owner-role broadcast where
// the mask passes every field through MUST NOT trigger an audit emit
// even if ShouldAuditWS would otherwise sample in. The contract emits
// only when at least one field was actually removed.
func TestHub_BroadcastMasked_NoAudit_OnOwnerNoStrip(t *testing.T) {
	emitter := &fakeAuditEmitter{}
	metrics := &fakeAuditMetrics{}

	hub := NewHub(slog.Default(), NoopHubMetrics{}, WithMaskAudit(emitter, metrics))
	t.Cleanup(hub.Stop)

	a := &testAuth{
		userID:        "user-1",
		vehicleIDs:    []string{"v-1"},
		roleByVehicle: map[string]auth.Role{"v-1": auth.RoleOwner},
	}
	srv := newTestServer(t, hub, a)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "valid-token")
	t.Cleanup(func() { _ = conn.CloseNow() })

	waitForClients(t, hub, 1)

	// Drive 1000 broadcasts; without any fields removed, audit emit
	// must stay at zero.
	payload := map[string]any{"speed": 65, "licensePlate": "ABC-123"}
	for range 1000 {
		hub.BroadcastMasked(
			"v-1",
			mask.ResourceVehicleState,
			time.Now().UTC().Format(time.RFC3339),
			payload,
		)
	}

	// Drain the connection so dropped messages don't backfill the
	// emitter via some unintended path. Read deadlines are short
	// because the goal is just to consume any pending frames.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	// Wait briefly for any goroutines to settle.
	time.Sleep(50 * time.Millisecond)

	if got := len(emitter.snapshot()); got != 0 {
		t.Errorf("audit emitted %d times for owner with no fields stripped; want 0", got)
	}
	if got := metrics.writes.Load(); got != 0 {
		t.Errorf("metrics.writes = %d, want 0 when nothing was masked", got)
	}
}

// TestHub_BroadcastMasked_AuditEmitFailure_DoesNotDropFrame guards the
// contract's non-blocking invariant: an InsertAuditLog error MUST log
// + increment the failure metric but MUST NOT prevent the masked frame
// from reaching the client.
func TestHub_BroadcastMasked_AuditEmitFailure_DoesNotDropFrame(t *testing.T) {
	emitter := &fakeAuditEmitter{err: errors.New("simulated DB failure")}
	metrics := &fakeAuditMetrics{}

	hub := NewHub(slog.Default(), NoopHubMetrics{}, WithMaskAudit(emitter, metrics))
	t.Cleanup(hub.Stop)

	a := &testAuth{
		userID:        "user-1",
		vehicleIDs:    []string{"v-1"},
		roleByVehicle: map[string]auth.Role{"v-1": auth.RoleViewer},
	}
	srv := newTestServer(t, hub, a)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "valid-token")
	t.Cleanup(func() { _ = conn.CloseNow() })

	waitForClients(t, hub, 1)

	// One broadcast that strips licensePlate; even if audit fails,
	// the viewer must receive the projected frame.
	hub.BroadcastMasked(
		"v-1",
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		map[string]any{
			"speed":        65,
			"licensePlate": "ABC-123",
		},
	)

	got := readMessage(t, conn)
	if got.Type != msgTypeVehicleUpdate {
		t.Fatalf("expected vehicle_update despite audit failure, got %q", got.Type)
	}
}

// TestHub_NextFrameSeq_PerVehicleMonotonic verifies the per-vehicle
// counter is independent across vehicles and monotonic within one
// vehicle. The audit sampler depends on this counter being unique per
// (vehicleID, frame) so the 1% sample distributes uniformly.
func TestHub_NextFrameSeq_PerVehicleMonotonic(t *testing.T) {
	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	if got := hub.nextFrameSeq("v-1"); got != 1 {
		t.Errorf("first nextFrameSeq for v-1 = %d, want 1", got)
	}
	if got := hub.nextFrameSeq("v-1"); got != 2 {
		t.Errorf("second nextFrameSeq for v-1 = %d, want 2", got)
	}
	// Distinct vehicles must keep distinct counters.
	if got := hub.nextFrameSeq("v-2"); got != 1 {
		t.Errorf("first nextFrameSeq for v-2 = %d, want 1 (independent counter)", got)
	}
	if got := hub.nextFrameSeq("v-1"); got != 3 {
		t.Errorf("third nextFrameSeq for v-1 = %d, want 3", got)
	}
}
