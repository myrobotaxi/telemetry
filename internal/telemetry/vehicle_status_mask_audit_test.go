package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
	"github.com/tnando/my-robo-taxi-telemetry/internal/mask"
)

// fakeAuditEmitter is a recording AuditEmitter for REST mask tests.
// Each test owns its own instance (no global state).
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

// fakeAuditMetrics counts mask audit emits.
type fakeAuditMetrics struct {
	writes   atomic.Int64
	failures atomic.Int64
}

func (f *fakeAuditMetrics) IncAuditWrite(string, string)        { f.writes.Add(1) }
func (f *fakeAuditMetrics) IncAuditWriteFailure(string, string) { f.failures.Add(1) }

// waitForEntries polls f.snapshot() until it reaches at least want
// entries or the deadline expires. EmitAsync runs on a goroutine so
// the test must wait — a fixed Sleep would either be too short or
// drag tests out unnecessarily.
func waitForEntries(t *testing.T, f *fakeAuditEmitter, want int) {
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
			t.Fatalf("timed out waiting for %d audit entries, got %d", want, len(f.snapshot()))
		case <-tick.C:
		}
	}
}

// findSamplingRequestID discovers an X-Request-ID value that ShouldAuditREST
// will sample IN for the given (userID, vehicleID). The test just iterates
// integers as request IDs until it finds one — this avoids the test
// depending on a specific FNV outcome.
func findSamplingRequestID(t *testing.T, userID, vehicleID string) string {
	t.Helper()
	for i := 0; i < 5000; i++ {
		candidate := "req-" + strconv.Itoa(i)
		if mask.ShouldAuditREST(userID, candidate, vehicleID) {
			return candidate
		}
	}
	t.Fatalf("no sampling-in request ID found for (user=%q, veh=%q)", userID, vehicleID)
	return ""
}

// TestVehicleStatus_MaskedResponse_EmitsAudit_OnViewerStrip drives a
// VehicleStatusHandler with a viewer-role caller through the mask
// pipeline. Even though the connectivity-probe shape is wider than
// the vehicle_state allow-list, the test only verifies the audit
// path: at least one field is stripped, and when the X-Request-ID
// hashes into the 1% sample bucket, an AuditEntry lands.
func TestVehicleStatus_MaskedResponse_EmitsAudit_OnViewerStrip(t *testing.T) {
	emitter := &fakeAuditEmitter{}
	metrics := &fakeAuditMetrics{}

	const (
		userID    = "user-1"
		vehicleID = "veh-1"
	)
	requestID := findSamplingRequestID(t, userID, vehicleID)

	handler := NewVehicleStatusHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleOwner{ownerID: userID},
		&stubVehiclePresence{connected: true},
		discardLogger(),
		WithMask(
			mask.ResourceVehicleState,
			&stubRoleResolver{role: auth.RoleViewer},
			&stubVehicleIDLookup{id: vehicleID},
		),
		WithMaskAudit(emitter, metrics, "/api/vehicles/{vehicleId}/snapshot"),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicle-status/{vin}", handler)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicle-status/5YJ3E1EA1PF000001",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("X-Request-ID", requestID)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	waitForEntries(t, emitter, 1)
	got := emitter.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 entry, got %d", len(got))
	}
	entry := got[0]
	if entry.Action != "mask_applied" {
		t.Errorf("Action = %q, want mask_applied", entry.Action)
	}
	if entry.TargetType != string(mask.TargetRESTResponse) {
		t.Errorf("TargetType = %q, want rest_response", entry.TargetType)
	}
	if entry.TargetID != vehicleID {
		t.Errorf("TargetID = %q, want %q", entry.TargetID, vehicleID)
	}
	if entry.UserID != userID {
		t.Errorf("UserID = %q, want %q (REST audit row IS per-caller)", entry.UserID, userID)
	}

	var meta map[string]any
	if err := json.Unmarshal(entry.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta["role"] != string(auth.RoleViewer) {
		t.Errorf("metadata.role = %v, want viewer", meta["role"])
	}
	if meta["channel"] != string(mask.AuditChannelREST) {
		t.Errorf("metadata.channel = %v, want rest", meta["channel"])
	}
	if meta["endpoint"] != "/api/vehicles/{vehicleId}/snapshot" {
		t.Errorf("metadata.endpoint = %v, want route pattern", meta["endpoint"])
	}
	fields, ok := meta["fieldsMasked"].([]any)
	if !ok || len(fields) == 0 {
		t.Errorf("metadata.fieldsMasked = %v, want non-empty list", meta["fieldsMasked"])
	}

	if metrics.writes.Load() < 1 {
		t.Errorf("expected at least 1 metric write, got %d", metrics.writes.Load())
	}
}

// TestVehicleStatus_MaskedResponse_NoAudit_OnNoSample drives the
// handler with a request whose (userID, requestID, vehicleID) does
// NOT sample in. No audit row should land regardless of how many
// fields the mask stripped.
func TestVehicleStatus_MaskedResponse_NoAudit_OnNoSample(t *testing.T) {
	emitter := &fakeAuditEmitter{}
	metrics := &fakeAuditMetrics{}

	const (
		userID    = "user-1"
		vehicleID = "veh-1"
	)

	// Find a request ID that does NOT sample in.
	notSampling := ""
	for i := 0; i < 100; i++ {
		candidate := "noreq-" + strconv.Itoa(i)
		if !mask.ShouldAuditREST(userID, candidate, vehicleID) {
			notSampling = candidate
			break
		}
	}
	if notSampling == "" {
		t.Fatal("no non-sampling request ID found")
	}

	handler := NewVehicleStatusHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleOwner{ownerID: userID},
		&stubVehiclePresence{connected: true},
		discardLogger(),
		WithMask(
			mask.ResourceVehicleState,
			&stubRoleResolver{role: auth.RoleViewer},
			&stubVehicleIDLookup{id: vehicleID},
		),
		WithMaskAudit(emitter, metrics, "/api/vehicles/{vehicleId}/snapshot"),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicle-status/{vin}", handler)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicle-status/5YJ3E1EA1PF000001",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("X-Request-ID", notSampling)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	// Wait briefly to catch any async writes.
	time.Sleep(50 * time.Millisecond)
	if got := len(emitter.snapshot()); got != 0 {
		t.Errorf("expected 0 audit entries on non-sampling request, got %d", got)
	}
	if got := metrics.writes.Load(); got != 0 {
		t.Errorf("metrics.writes = %d, want 0 on non-sampling request", got)
	}
}

// TestVehicleStatus_MaskedResponse_AuditFailure_DoesNotBreakResponse
// confirms an InsertAuditLog error logs + increments the failure
// metric but the 200 response still goes out.
func TestVehicleStatus_MaskedResponse_AuditFailure_DoesNotBreakResponse(t *testing.T) {
	emitter := &fakeAuditEmitter{err: errors.New("simulated DB failure")}
	metrics := &fakeAuditMetrics{}

	const (
		userID    = "user-1"
		vehicleID = "veh-1"
	)
	requestID := findSamplingRequestID(t, userID, vehicleID)

	handler := NewVehicleStatusHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleOwner{ownerID: userID},
		&stubVehiclePresence{connected: true},
		discardLogger(),
		WithMask(
			mask.ResourceVehicleState,
			&stubRoleResolver{role: auth.RoleViewer},
			&stubVehicleIDLookup{id: vehicleID},
		),
		WithMaskAudit(emitter, metrics, "/api/vehicles/{vehicleId}/snapshot"),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicle-status/{vin}", handler)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicle-status/5YJ3E1EA1PF000001",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("X-Request-ID", requestID)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 even with audit failure", rec.Code)
	}

	waitForFailure(t, metrics)
	if got := metrics.writes.Load(); got != 0 {
		t.Errorf("metrics.writes = %d, want 0 on failure", got)
	}
}

func waitForFailure(t *testing.T, m *fakeAuditMetrics) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if m.failures.Load() >= 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for failure metric")
		case <-tick.C:
		}
	}
}

// TestRequestIDFromRequest exercises the X-Request-ID header
// fallback. When the client supplies the header, it is echoed; when
// absent, a server-generated random ID is returned (32-char hex).
func TestRequestIDFromRequest(t *testing.T) {
	tests := []struct {
		name      string
		hdr       string
		wantExact string // if non-empty, expect exact match
		wantHex32 bool   // if true, expect 32-char hex
	}{
		{name: "client supplied header", hdr: "client-req-1", wantExact: "client-req-1"},
		{name: "missing header generates 32-char hex", hdr: "", wantHex32: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			if tt.hdr != "" {
				req.Header.Set("X-Request-ID", tt.hdr)
			}
			got := requestIDFromRequest(req)
			if tt.wantExact != "" && got != tt.wantExact {
				t.Errorf("got %q, want %q", got, tt.wantExact)
			}
			if tt.wantHex32 {
				if len(got) != 32 {
					t.Errorf("generated id length = %d, want 32", len(got))
				}
			}
		})
	}
}

// TestVehicleStatus_NoAuditEmitter_NoCrash verifies the backward-
// compatible path: WithMaskAudit not supplied (auditEmitter is nil),
// the handler still serves the masked response without panic.
func TestVehicleStatus_NoAuditEmitter_NoCrash(t *testing.T) {
	const (
		userID    = "user-1"
		vehicleID = "veh-1"
	)

	handler := NewVehicleStatusHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleOwner{ownerID: userID},
		&stubVehiclePresence{connected: true},
		discardLogger(),
		WithMask(
			mask.ResourceVehicleState,
			&stubRoleResolver{role: auth.RoleViewer},
			&stubVehicleIDLookup{id: vehicleID},
		),
		// No WithMaskAudit.
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicle-status/{vin}", handler)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicle-status/5YJ3E1EA1PF000001",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
}
