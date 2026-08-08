package store_test

// Integration tests for the MYR-447 operator-access audit.
//
// Isolation note: "AuditLog" is append-only — its triggers refuse DELETE —
// so these tests CANNOT clean up after themselves. Every test therefore
// scopes its assertions to a per-test-unique userId (uniqueSubjectID) and
// queries only rows carrying it. Never add a DELETE here; it will fail
// with "AuditLog rows are append-only".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// subjectCounter makes uniqueSubjectID collision-free within a process;
// the timestamp component separates repeated `go test -count=N` runs
// against a reused container.
var subjectCounter atomic.Int64

func uniqueSubjectID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("subj_%d_%d", time.Now().UnixNano(), subjectCounter.Add(1))
}

// operatorRow is one AuditLog row read back for assertions.
type operatorRow struct {
	action     string
	targetType string
	targetID   string
	initiator  string
	metadata   []byte
}

// readOperatorRows returns every AuditLog row for the given data subject.
func readOperatorRows(t *testing.T, userID string) []operatorRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT "action", "targetType", "targetId", "initiator", "metadata"
		   FROM "AuditLog" WHERE "userId" = $1 ORDER BY "timestamp"`, userID)
	if err != nil {
		t.Fatalf("query AuditLog: %v", err)
	}
	defer rows.Close()

	var out []operatorRow
	for rows.Next() {
		var r operatorRow
		if err := rows.Scan(&r.action, &r.targetType, &r.targetID, &r.initiator, &r.metadata); err != nil {
			t.Fatalf("scan AuditLog row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate AuditLog rows: %v", err)
	}
	return out
}

// validAccess returns a fully-populated OperatorAccess for the subject.
func validAccess(subject string) store.OperatorAccess {
	return store.OperatorAccess{
		Operator:   "jdoe",
		Command:    "ops auth token",
		UserID:     subject,
		TargetType: store.OperatorTargetUser,
		TargetID:   subject,
		Fields:     []string{"refreshToken", "accessToken"},
	}
}

func TestOperatorAuditor_RecordDecryptWritesOneRow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping operator audit integration test")
	}
	ensureAuditSchema(t)

	auditor := store.NewOperatorAuditor(testPool)
	ctx := context.Background()

	tests := []struct {
		name           string
		mutate         func(*store.OperatorAccess)
		wantTargetType string
	}{
		{
			name:           "user target — ops auth token",
			mutate:         func(*store.OperatorAccess) {},
			wantTargetType: "user",
		},
		{
			name: "vehicle target — ops fields snapshot",
			mutate: func(a *store.OperatorAccess) {
				a.Command = "ops fields snapshot"
				a.TargetType = store.OperatorTargetVehicle
				a.TargetID = "cveh0001"
				a.Fields = []string{"latitude", "longitude", "navRouteCoordinates"}
			},
			wantTargetType: "vehicle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subject := uniqueSubjectID(t)
			access := validAccess(subject)
			tt.mutate(&access)

			if err := auditor.RecordDecrypt(ctx, access); err != nil {
				t.Fatalf("RecordDecrypt: unexpected error: %v", err)
			}

			rows := readOperatorRows(t, subject)
			if len(rows) != 1 {
				t.Fatalf("expected exactly 1 audit row, got %d", len(rows))
			}
			got := rows[0]
			if got.action != "operator_decrypt" {
				t.Errorf("action = %q, want operator_decrypt", got.action)
			}
			if got.initiator != "operator" {
				t.Errorf("initiator = %q, want operator", got.initiator)
			}
			if got.targetType != tt.wantTargetType {
				t.Errorf("targetType = %q, want %q", got.targetType, tt.wantTargetType)
			}
			if got.targetID != access.TargetID {
				t.Errorf("targetId = %q, want %q", got.targetID, access.TargetID)
			}
		})
	}
}

// TestOperatorAuditor_MetadataIsP0Only is the CG-DL-5 guard. It records a
// decrypt whose field NAMES are exactly the ones `ops fields snapshot`
// exposes, then asserts the persisted metadata contains the names, the
// operator, the command and a count — and none of the VALUES those names
// stood for.
func TestOperatorAuditor_MetadataIsP0Only(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping operator audit integration test")
	}
	ensureAuditSchema(t)

	subject := uniqueSubjectID(t)
	access := store.OperatorAccess{
		Operator:   "jdoe",
		Command:    "ops fields snapshot",
		UserID:     subject,
		TargetType: store.OperatorTargetVehicle,
		TargetID:   "cveh0002",
		Fields: []string{
			"longitude", "latitude", "locationName", "locationAddress",
			"destinationAddress", "navRouteCoordinates",
		},
	}
	if err := store.NewOperatorAuditor(testPool).RecordDecrypt(context.Background(), access); err != nil {
		t.Fatalf("RecordDecrypt: unexpected error: %v", err)
	}

	rows := readOperatorRows(t, subject)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 audit row, got %d", len(rows))
	}
	raw := string(rows[0].metadata)

	var meta struct {
		Command    string   `json:"command"`
		Operator   string   `json:"operator"`
		Fields     []string `json:"fields"`
		FieldCount int      `json:"fieldCount"`
	}
	if err := json.Unmarshal(rows[0].metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata %s: %v", raw, err)
	}

	if meta.Command != access.Command {
		t.Errorf("metadata.command = %q, want %q", meta.Command, access.Command)
	}
	if meta.Operator != access.Operator {
		t.Errorf("metadata.operator = %q, want %q", meta.Operator, access.Operator)
	}
	if meta.FieldCount != len(access.Fields) {
		t.Errorf("metadata.fieldCount = %d, want %d", meta.FieldCount, len(access.Fields))
	}
	if len(meta.Fields) != len(access.Fields) {
		t.Fatalf("metadata.fields = %v, want %d entries", meta.Fields, len(access.Fields))
	}
	// Names are normalized to sorted order so rows compare cleanly.
	for i := 1; i < len(meta.Fields); i++ {
		if meta.Fields[i-1] > meta.Fields[i] {
			t.Errorf("metadata.fields not sorted: %v", meta.Fields)
			break
		}
	}

	// The P0 assertion: no decrypted VALUE of any kind appears in the row.
	// These are representative plaintexts of exactly the fields named above.
	forbidden := []string{
		"37.7749", "-122.4194", // coordinates
		"1600 Amphitheatre",      // street address
		"Home", "Ferry Building", // place labels
		"eyJhbGciOiJSUzI1NiIsInR5cCI6", // a Tesla JWT prefix
		"@",                            // an operator email would be P1
	}
	for _, bad := range forbidden {
		if strings.Contains(raw, bad) {
			t.Errorf("metadata contains non-P0 value %q: %s", bad, raw)
		}
	}
}

// TestOperatorAuditor_RejectsUnattributableAccess covers the fail-closed
// input guards: a decrypt that cannot be attributed, or that would put a
// value into a P0 column, is refused and NOTHING is written.
func TestOperatorAuditor_RejectsUnattributableAccess(t *testing.T) {
	if !dockerAvailable {
		t.Skip("Docker not available; skipping operator audit integration test")
	}
	ensureAuditSchema(t)

	auditor := store.NewOperatorAuditor(testPool)
	ctx := context.Background()

	tests := []struct {
		name    string
		mutate  func(*store.OperatorAccess)
		wantErr error
	}{
		{
			name:    "blank operator handle is rejected",
			mutate:  func(a *store.OperatorAccess) { a.Operator = "" },
			wantErr: store.ErrOperatorHandleRequired,
		},
		{
			name:    "whitespace-only operator handle is rejected",
			mutate:  func(a *store.OperatorAccess) { a.Operator = "   " },
			wantErr: store.ErrOperatorHandleRequired,
		},
		{
			name:    "email as operator handle is rejected (P1)",
			mutate:  func(a *store.OperatorAccess) { a.Operator = "j.doe@example.com" },
			wantErr: store.ErrOperatorHandleRequired,
		},
		{
			name:    "empty command is rejected",
			mutate:  func(a *store.OperatorAccess) { a.Command = "" },
			wantErr: store.ErrOperatorAccessInvalid,
		},
		{
			name:    "empty targetType is rejected",
			mutate:  func(a *store.OperatorAccess) { a.TargetType = "" },
			wantErr: store.ErrOperatorAccessInvalid,
		},
		{
			name:    "empty field list is rejected",
			mutate:  func(a *store.OperatorAccess) { a.Fields = nil },
			wantErr: store.ErrOperatorAccessInvalid,
		},
		{
			name:    "a coordinate smuggled in as a field name is rejected",
			mutate:  func(a *store.OperatorAccess) { a.Fields = []string{"37.7749"} },
			wantErr: store.ErrOperatorAccessInvalid,
		},
		{
			name:    "an address smuggled in as a field name is rejected",
			mutate:  func(a *store.OperatorAccess) { a.Fields = []string{"1600 Amphitheatre Pkwy"} },
			wantErr: store.ErrOperatorAccessInvalid,
		},
		{
			name: "a token smuggled in as a field name is rejected",
			mutate: func(a *store.OperatorAccess) {
				a.Fields = []string{strings.Repeat("eyJhbGciOiJSUzI1NiJ9", 8)}
			},
			wantErr: store.ErrOperatorAccessInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subject := uniqueSubjectID(t)
			access := validAccess(subject)
			tt.mutate(&access)

			err := auditor.RecordDecrypt(ctx, access)
			if err == nil {
				t.Fatalf("RecordDecrypt: expected error, got nil")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("RecordDecrypt error = %v, want errors.Is(_, %v)", err, tt.wantErr)
			}
			if rows := readOperatorRows(t, subject); len(rows) != 0 {
				t.Errorf("rejected access still wrote %d audit row(s) — must be fail-closed", len(rows))
			}
		})
	}
}

// TestOperatorAuditor_NotConfigured proves a miswired auditor fails closed
// rather than panicking or silently no-oping.
func TestOperatorAuditor_NotConfigured(t *testing.T) {
	err := store.NewOperatorAuditor(nil).RecordDecrypt(context.Background(), validAccess("subj_nil"))
	if !errors.Is(err, store.ErrOperatorAuditorNotConfigured) {
		t.Errorf("RecordDecrypt error = %v, want errors.Is(_, ErrOperatorAuditorNotConfigured)", err)
	}
}
