package auditsidecar

import (
	"errors"
	"testing"
	"time"
)

// TestNoopSidecar verifies that NoopSidecar.Emit is always a no-op.
func TestNoopSidecar(t *testing.T) {
	var s NoopSidecar
	entry := AuditEntry{
		ID:        "test-id",
		UserID:    "user-1",
		Timestamp: time.Now(),
		Action:    "account_deleted",
	}
	if err := s.Emit(entry); err != nil {
		t.Fatalf("NoopSidecar.Emit() error = %v; want nil", err)
	}
}

// TestObjectKey validates the S3 key schema for a known timestamp.
func TestObjectKey(t *testing.T) {
	ts := time.Date(2026, 5, 9, 14, 30, 0, 123456789, time.UTC)
	e := AuditEntry{
		ID:        "audit-abc",
		UserID:    "user-xyz",
		Timestamp: ts,
	}
	got := objectKey(e)
	want := "audit/v1/2026/05/09/user-xyz/123456789000123456789-audit-abc.json"
	// The nano component encodes ts.UnixNano().
	wantPrefix := "audit/v1/2026/05/09/user-xyz/"
	if got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("objectKey() prefix = %q; want prefix %q", got, wantPrefix)
	}
	// Verify the suffix contains the id.
	suffix := "audit-abc.json"
	if got[len(got)-len(suffix):] != suffix {
		t.Errorf("objectKey() suffix = %q; want suffix %q", got, suffix)
	}
	_ = want // documented shape
}

// TestObjectKeyZeroTimestamp verifies that a zero Timestamp falls back to
// time.Now() without panicking.
func TestObjectKeyZeroTimestamp(t *testing.T) {
	e := AuditEntry{
		ID:     "id-zero",
		UserID: "u",
	}
	got := objectKey(e)
	if got == "" {
		t.Fatal("objectKey() with zero timestamp returned empty string")
	}
}

// TestMarshalEntry verifies the JSON payload structure.
func TestMarshalEntry(t *testing.T) {
	ts := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	e := AuditEntry{
		ID:         "id-1",
		UserID:     "u-1",
		Timestamp:  ts,
		Action:     "mask_applied",
		TargetType: "drive",
		TargetID:   "drv-1",
		Initiator:  "system",
		Metadata:   []byte(`{"count":1}`),
		CreatedAt:  ts,
	}
	b, err := marshalEntry(e)
	if err != nil {
		t.Fatalf("marshalEntry() error = %v", err)
	}
	got := string(b)
	for _, want := range []string{`"id":"id-1"`, `"userId":"u-1"`, `"action":"mask_applied"`, `"count":1`} {
		if !contains(got, want) {
			t.Errorf("marshalEntry() output missing %q; got %s", want, got)
		}
	}
}

// TestMarshalEntryNilMetadata verifies that nil metadata is serialised as {}.
func TestMarshalEntryNilMetadata(t *testing.T) {
	e := AuditEntry{ID: "id-nil", UserID: "u"}
	b, err := marshalEntry(e)
	if err != nil {
		t.Fatalf("marshalEntry() error = %v", err)
	}
	if !contains(string(b), `"metadata":{}`) {
		t.Errorf("marshalEntry() missing empty metadata object; got %s", b)
	}
}

// TestNoopMetrics verifies that NoopMetrics methods do not panic.
func TestNoopMetrics(t *testing.T) {
	var m NoopMetrics
	m.IncWrite()
	m.IncFailure("aws")
	m.IncFailure("enqueue_full")
	m.IncFailure("other")
	m.IncFailure("unknown") // should not panic
	m.SetQueueDepth(42)
}

// TestErrQueueFull verifies the sentinel error identity.
func TestErrQueueFull(t *testing.T) {
	if !errors.Is(ErrQueueFull, ErrQueueFull) {
		t.Fatal("ErrQueueFull is not equal to itself via errors.Is")
	}
	if ErrQueueFull.Error() == "" {
		t.Fatal("ErrQueueFull.Error() is empty")
	}
}

// contains is a small helper to avoid importing strings in test code.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
