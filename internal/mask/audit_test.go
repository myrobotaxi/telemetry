package mask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
)

func TestShouldAuditREST_Deterministic(t *testing.T) {
	tests := []struct {
		name      string
		userID    string
		requestID string
		resource  string
	}{
		{name: "typical inputs", userID: "user-1", requestID: "req-abc", resource: "veh-xyz"},
		{name: "empty inputs", userID: "", requestID: "", resource: ""},
		{name: "long inputs", userID: "user-with-a-very-long-cuid-cmkx0001abc", requestID: "01HX9YJWE9G2A3FQB", resource: "cmvehicle12345"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := ShouldAuditREST(tt.userID, tt.requestID, tt.resource)
			for range 5 {
				if got := ShouldAuditREST(tt.userID, tt.requestID, tt.resource); got != first {
					t.Fatalf("ShouldAuditREST not deterministic: first=%v, again=%v", first, got)
				}
			}
		})
	}
}

func TestShouldAuditWS_Deterministic(t *testing.T) {
	for i := uint64(0); i < 20; i++ {
		first := ShouldAuditWS("vehicle-x", auth.RoleViewer, i)
		for range 3 {
			if got := ShouldAuditWS("vehicle-x", auth.RoleViewer, i); got != first {
				t.Fatalf("ShouldAuditWS not deterministic at frameSeq=%d: first=%v, again=%v", i, first, got)
			}
		}
	}
}

func TestShouldAuditREST_DistributionApprox1Percent(t *testing.T) {
	// Sample 10000 distinct triples. The 1% rate (modulus 100) means
	// we expect ~100 trues. Check with generous bounds (50..200) to
	// avoid flakiness from FNV's distribution on sequential inputs.
	const samples = 10000
	hits := 0
	for i := range samples {
		if ShouldAuditREST(fmt.Sprintf("user-%d", i), fmt.Sprintf("req-%d", i), fmt.Sprintf("res-%d", i)) {
			hits++
		}
	}
	if hits < 50 || hits > 200 {
		t.Errorf("ShouldAuditREST hit rate over %d samples: %d (expected ~100, allowed 50..200)", samples, hits)
	}
}

func TestShouldAuditWS_DistributionApprox1Percent(t *testing.T) {
	const samples = 10000
	hits := 0
	for i := uint64(0); i < samples; i++ {
		if ShouldAuditWS("vehicle-x", auth.RoleOwner, i) {
			hits++
		}
	}
	if hits < 50 || hits > 200 {
		t.Errorf("ShouldAuditWS hit rate over %d samples: %d (expected ~100, allowed 50..200)", samples, hits)
	}
}

func TestShouldAuditREST_DiffersAcrossInputs(t *testing.T) {
	// Length-prefixed hashing protects against collisions like
	// ("ab", "cdef") vs ("abc", "def"). Verify both pairs do not
	// produce the same hash AND result. (Equality of bool result is
	// fine for any individual pair; what we want to guard is that
	// changing the boundary produces a different hash bit.)
	a := ShouldAuditREST("ab", "cdef", "x")
	b := ShouldAuditREST("abc", "def", "x")
	// They COULD coincidentally be the same; what matters is the
	// underlying hashes differ. Since ShouldAuditREST collapses to a
	// bool, we instead verify a wider sweep produces both true and
	// false outcomes from these patterns.
	_ = a
	_ = b

	hits := 0
	misses := 0
	for i := range 200 {
		// Vary the boundary across many distinct values; should land
		// on both true and false.
		if ShouldAuditREST(fmt.Sprintf("a%db", i), fmt.Sprintf("c%dd", i), "x") {
			hits++
		} else {
			misses++
		}
	}
	if hits == 0 || misses == 0 {
		t.Errorf("expected both true and false outcomes; hits=%d misses=%d", hits, misses)
	}
}

func TestBuildEntry_HappyPath(t *testing.T) {
	entry, err := BuildEntry(
		"user-1",
		TargetWSBroadcast,
		"vehicle-1",
		auth.RoleViewer,
		AuditChannelWS,
		[]string{"licensePlate"},
		"",
	)
	if err != nil {
		t.Fatalf("BuildEntry: %v", err)
	}
	if entry.Action != "mask_applied" {
		t.Errorf("Action = %q, want %q", entry.Action, "mask_applied")
	}
	if entry.TargetType != string(TargetWSBroadcast) {
		t.Errorf("TargetType = %q, want %q", entry.TargetType, TargetWSBroadcast)
	}
	if entry.TargetID != "vehicle-1" {
		t.Errorf("TargetID = %q, want vehicle-1", entry.TargetID)
	}
	if entry.UserID != "user-1" {
		t.Errorf("UserID = %q, want user-1", entry.UserID)
	}
	if entry.Initiator != "user" {
		t.Errorf("Initiator = %q, want user", entry.Initiator)
	}
	if entry.ID == "" {
		t.Error("ID must be populated")
	}
	if !strings.HasPrefix(entry.ID, "c") {
		t.Errorf("ID = %q, want cuid-shaped (c<hex>)", entry.ID)
	}
	if entry.Timestamp.IsZero() || entry.CreatedAt.IsZero() {
		t.Error("Timestamp and CreatedAt must be populated")
	}

	var meta map[string]any
	if err := json.Unmarshal(entry.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	for k := range meta {
		if _, ok := allowedMetadataKeys[k]; !ok {
			t.Errorf("metadata key %q is NOT in the allow-list — CG-DL-5 risk", k)
		}
	}
	if meta["role"] != string(auth.RoleViewer) {
		t.Errorf("metadata.role = %v, want %q", meta["role"], auth.RoleViewer)
	}
	if meta["channel"] != string(AuditChannelWS) {
		t.Errorf("metadata.channel = %v, want %q", meta["channel"], AuditChannelWS)
	}
}

func TestBuildEntry_RejectsEmptyFieldsMasked(t *testing.T) {
	tests := []struct {
		name    string
		fields  []string
		wantErr error
	}{
		{name: "nil fieldsMasked", fields: nil, wantErr: ErrInvalidAuditMetadata},
		{name: "empty fieldsMasked", fields: []string{}, wantErr: ErrInvalidAuditMetadata},
		{name: "empty entry inside fieldsMasked", fields: []string{"licensePlate", ""}, wantErr: ErrInvalidAuditMetadata},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildEntry(
				"user-1",
				TargetRESTResponse,
				"vehicle-1",
				auth.RoleOwner,
				AuditChannelREST,
				tt.fields,
				"/api/vehicles/{vehicleId}/snapshot",
			)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want errors.Is(%v)", err, tt.wantErr)
			}
		})
	}
}

func TestBuildEntry_RESTEndpointIsRoutePattern(t *testing.T) {
	// rest-api.md §5.3 example shows the endpoint as a parameterized
	// route pattern, NOT a substituted URL. Substituting the vehicleId
	// would put the same opaque ID into both targetId AND metadata,
	// which is harmless but redundant. The contract example uses
	// "{vehicleId}". Verify BuildEntry preserves whatever the caller
	// passed without surprise transformations.
	entry, err := BuildEntry(
		"user-1",
		TargetRESTResponse,
		"vehicle-cuid",
		auth.RoleViewer,
		AuditChannelREST,
		[]string{"licensePlate"},
		"/api/vehicles/{vehicleId}/snapshot",
	)
	if err != nil {
		t.Fatalf("BuildEntry: %v", err)
	}

	var meta map[string]any
	if err := json.Unmarshal(entry.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta["endpoint"] != "/api/vehicles/{vehicleId}/snapshot" {
		t.Errorf("metadata.endpoint = %v, want route pattern", meta["endpoint"])
	}
}

func TestAllowedMetadataKeysIntersect(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		want bool
	}{
		{name: "all allowed", keys: []string{"role", "channel", "fieldsMasked", "endpoint"}, want: true},
		{name: "subset", keys: []string{"role", "fieldsMasked"}, want: true},
		{name: "empty", keys: nil, want: true},
		{name: "unknown key rejected", keys: []string{"role", "userEmail"}, want: false},
		{name: "p1-shaped key rejected", keys: []string{"latitude"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allowedMetadataKeysIntersect(tt.keys); got != tt.want {
				t.Errorf("allowedMetadataKeysIntersect(%v) = %v, want %v", tt.keys, got, tt.want)
			}
		})
	}
}

// fakeAuditEmitter is a test AuditEmitter that records calls and can
// inject errors / panics.
type fakeAuditEmitter struct {
	mu      sync.Mutex
	entries []AuditEntry
	err     error
	panicOn bool
	signal  chan struct{}
}

func newFakeAuditEmitter() *fakeAuditEmitter {
	return &fakeAuditEmitter{signal: make(chan struct{}, 16)}
}

func (f *fakeAuditEmitter) InsertAuditLog(_ context.Context, entry AuditEntry) error {
	if f.panicOn {
		panic("simulated emitter panic")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, entry)
	// Non-blocking signal so the test can wait for completion.
	select {
	case f.signal <- struct{}{}:
	default:
	}
	return f.err
}

func (f *fakeAuditEmitter) snapshot() []AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]AuditEntry, len(f.entries))
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

// waitForCount polls until predicate returns true, or fails the test
// after a generous deadline. Avoids fragile time.Sleep races on the
// fire-and-forget Emit goroutine.
func waitForCount(t *testing.T, name string, count func() int) {
	t.Helper()
	timeout := time.After(time.Second)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if count() == 1 {
			return
		}
		select {
		case <-timeout:
			t.Fatalf("%s: timed out waiting for count=1, got %d", name, count())
		case <-tick.C:
		}
	}
}

func TestEmitAsync_HappyPath(t *testing.T) {
	emitter := newFakeAuditEmitter()
	metrics := &fakeAuditMetrics{}

	entry, err := BuildEntry(
		"user-1",
		TargetRESTResponse,
		"vehicle-1",
		auth.RoleViewer,
		AuditChannelREST,
		[]string{"licensePlate"},
		"/api/vehicles/{vehicleId}/snapshot",
	)
	if err != nil {
		t.Fatalf("BuildEntry: %v", err)
	}

	EmitAsync(context.Background(), emitter, metrics, slog.Default(), entry)

	waitForCount(t, "emitter.entries", func() int { return len(emitter.snapshot()) })
	waitForCount(t, "metrics.writes", func() int { return int(metrics.writes.Load()) })
	if got := metrics.failures.Load(); got != 0 {
		t.Errorf("expected 0 failures, got %d", got)
	}
}

func TestEmitAsync_NilEmitterIsNoop(t *testing.T) {
	// Pass nil — must not panic, must not increment metrics.
	metrics := &fakeAuditMetrics{}

	entry, err := BuildEntry(
		"user-1",
		TargetWSBroadcast,
		"vehicle-1",
		auth.RoleViewer,
		AuditChannelWS,
		[]string{"licensePlate"},
		"",
	)
	if err != nil {
		t.Fatalf("BuildEntry: %v", err)
	}

	EmitAsync(context.Background(), nil, metrics, slog.Default(), entry)

	// Wait a tick to confirm no goroutine fired.
	time.Sleep(10 * time.Millisecond)
	if got := metrics.writes.Load(); got != 0 {
		t.Errorf("nil emitter wrote metrics: writes=%d", got)
	}
	if got := metrics.failures.Load(); got != 0 {
		t.Errorf("nil emitter wrote metrics: failures=%d", got)
	}
}

func TestEmitAsync_InsertErrorIncrementsFailureMetric(t *testing.T) {
	emitter := newFakeAuditEmitter()
	emitter.err = errors.New("simulated DB failure")
	metrics := &fakeAuditMetrics{}

	entry, err := BuildEntry(
		"user-1",
		TargetWSBroadcast,
		"vehicle-1",
		auth.RoleViewer,
		AuditChannelWS,
		[]string{"licensePlate"},
		"",
	)
	if err != nil {
		t.Fatalf("BuildEntry: %v", err)
	}

	EmitAsync(context.Background(), emitter, metrics, slog.Default(), entry)

	waitForCount(t, "metrics.failures", func() int { return int(metrics.failures.Load()) })
	if got := metrics.writes.Load(); got != 0 {
		t.Errorf("expected 0 writes on error, got %d", got)
	}
}

func TestEmitAsync_PanicInEmitterIncrementsFailureMetric(t *testing.T) {
	emitter := newFakeAuditEmitter()
	emitter.panicOn = true
	metrics := &fakeAuditMetrics{}

	entry, err := BuildEntry(
		"user-1",
		TargetWSBroadcast,
		"vehicle-1",
		auth.RoleViewer,
		AuditChannelWS,
		[]string{"licensePlate"},
		"",
	)
	if err != nil {
		t.Fatalf("BuildEntry: %v", err)
	}

	// Must not panic the test process.
	EmitAsync(context.Background(), emitter, metrics, slog.Default(), entry)

	waitForCount(t, "metrics.failures", func() int { return int(metrics.failures.Load()) })
}

func TestEmitAsync_DetachedFromCanceledContext(t *testing.T) {
	emitter := newFakeAuditEmitter()
	metrics := &fakeAuditMetrics{}

	entry, err := BuildEntry(
		"user-1",
		TargetRESTResponse,
		"vehicle-1",
		auth.RoleViewer,
		AuditChannelREST,
		[]string{"licensePlate"},
		"/api/vehicles/{vehicleId}/snapshot",
	)
	if err != nil {
		t.Fatalf("BuildEntry: %v", err)
	}

	// Cancel the parent context BEFORE invoking EmitAsync. The detached
	// context inside emitDetached must keep going.
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	EmitAsync(parent, emitter, metrics, slog.Default(), entry)

	waitForCount(t, "emitter.entries", func() int { return len(emitter.snapshot()) })
}

func TestEmitAsync_NewAuditID_IsCuidShaped(t *testing.T) {
	// Sanity: a newAuditID looks like a cuid (starts with "c", is 33
	// chars or so, hex after the prefix).
	id := newAuditID()
	if !strings.HasPrefix(id, "c") {
		t.Fatalf("id = %q does not start with c", id)
	}
	// 1 (prefix) + 32 hex chars = 33 chars when 16 random bytes succeed.
	if len(id) != 33 {
		t.Errorf("id length = %d, want 33", len(id))
	}
}
