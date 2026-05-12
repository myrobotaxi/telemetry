package store_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestMaskAuditEmitter_EndToEnd is the MYR-71 integration test that
// confirms the mask audit pipeline lands rows on a real Postgres.
// Drives ~25 deterministic mask events through the
// EmitAsync -> MaskAuditEmitter -> AuditRepo path; the 1% sampling
// is hand-driven (we always sample in) so the assertion can be a
// strict count match. The contract correctness of the 1% rate is
// covered by the unit tests in internal/mask/audit_test.go.
//
// The integration value here is:
//
//   - The cross-package AuditEntry conversion in
//     store.MaskAuditEmitter actually compiles and inserts.
//   - Every column the contract documents (data-lifecycle.md §4.1)
//     persists with the expected value type after a real round-trip.
//   - The metadata JSONB column handles the canonical mask payload
//     shape (role / channel / fieldsMasked / endpoint) and survives
//     re-decoding.
func TestMaskAuditEmitter_EndToEnd(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping mask audit integration test")
	}
	ensureAuditSchema(t)
	cleanAuditLog(t, testPool)

	repo := store.NewAuditRepo(testPool)
	emitter := store.NewMaskAuditEmitter(repo)
	metrics := &countingAuditMetrics{}

	const samples = 25
	wantBy := make(map[string]struct{}, samples)
	for i := 0; i < samples; i++ {
		entry, err := mask.BuildEntry(
			"user-"+strconv.Itoa(i),
			mask.TargetWSBroadcast,
			"vehicle-"+strconv.Itoa(i%5),
			auth.RoleViewer,
			mask.AuditChannelWS,
			[]string{"licensePlate"},
			"",
		)
		if err != nil {
			t.Fatalf("BuildEntry: %v", err)
		}
		wantBy[entry.ID] = struct{}{}
		mask.EmitAsync(context.Background(), emitter, metrics, slog.Default(), entry)
	}

	// Wait for every async write to settle. The metric is the
	// safest signal — it ticks ONLY after InsertAuditLog returns.
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for metrics.Writes()+metrics.Failures() < int64(samples) {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for writes: writes=%d failures=%d", metrics.Writes(), metrics.Failures())
		case <-tick.C:
		}
	}
	if metrics.Failures() != 0 {
		t.Fatalf("unexpected emit failures: %d", metrics.Failures())
	}

	// Read back exactly the rows we wrote and assert shape.
	ctx := context.Background()
	rows, err := testPool.Query(ctx,
		`SELECT "id", "userId", "action", "targetType", "targetId", "initiator", "metadata"
		 FROM "AuditLog"
		 WHERE "action" = 'mask_applied' AND "targetType" = 'ws_broadcast'`)
	if err != nil {
		t.Fatalf("query AuditLog: %v", err)
	}
	defer rows.Close()

	gotBy := map[string]struct{}{}
	for rows.Next() {
		var (
			id, userID, action, targetType, targetID, initiator string
			metaRaw                                             json.RawMessage
		)
		if err := rows.Scan(&id, &userID, &action, &targetType, &targetID, &initiator, &metaRaw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if action != "mask_applied" {
			t.Errorf("action = %q, want mask_applied", action)
		}
		if targetType != "ws_broadcast" {
			t.Errorf("targetType = %q, want ws_broadcast", targetType)
		}
		if initiator != "user" {
			t.Errorf("initiator = %q, want user", initiator)
		}

		var meta map[string]any
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		// CG-DL-5 keys-only check — every key must come from the
		// allow-list in the mask package's BuildEntry contract.
		for k := range meta {
			switch k {
			case "role", "channel", "fieldsMasked", "endpoint":
				// allowed
			default:
				t.Errorf("metadata key %q not in CG-DL-5 allow-list", k)
			}
		}
		if meta["role"] != "viewer" {
			t.Errorf("metadata.role = %v, want viewer", meta["role"])
		}
		if meta["channel"] != "ws" {
			t.Errorf("metadata.channel = %v, want ws", meta["channel"])
		}

		gotBy[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iter: %v", err)
	}

	if len(gotBy) != samples {
		t.Fatalf("row count = %d, want %d", len(gotBy), samples)
	}
	for id := range wantBy {
		if _, ok := gotBy[id]; !ok {
			t.Errorf("missing inserted id: %s", id)
		}
	}
}

// countingAuditMetrics is a thread-safe AuditMetrics that counts
// successful writes and failures. Used as the test-side observer on
// the EmitAsync goroutine so the loop above can wait for completion
// deterministically (no Sleep races).
type countingAuditMetrics struct {
	mu       sync.Mutex
	writes   int64
	failures int64
}

func (c *countingAuditMetrics) IncAuditWrite(string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
}

func (c *countingAuditMetrics) IncAuditWriteFailure(string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
}

func (c *countingAuditMetrics) Writes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func (c *countingAuditMetrics) Failures() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures
}
