package main

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// fakeDecryptRecorder captures the operator_decrypt rows the backfill
// would write, and can fail on the Nth call to prove the fail-closed
// abort.
type fakeDecryptRecorder struct {
	got     []store.OperatorAccess
	failOn  int // 1-based call index that returns failErr; 0 = never fail
	failErr error
}

func (f *fakeDecryptRecorder) RecordDecrypt(_ context.Context, access store.OperatorAccess) error {
	f.got = append(f.got, access)
	if f.failOn > 0 && len(f.got) == f.failOn {
		return f.failErr
	}
	return nil
}

// TestAuditGeocodeBackfill covers the MYR-447 grouping contract: one
// operator_decrypt row per (owner, vehicle) per run — never one per drive,
// which would flood the append-only AuditLog on a fleet-wide sweep — and a
// hard abort the moment an insert fails, since the caller must not
// transmit a coordinate to Mapbox for an access it could not record.
func TestAuditGeocodeBackfill(t *testing.T) {
	rows := []store.DriveBackfillRow{
		{ID: "drv_1", VehicleID: "veh_a", UserID: "user_alpha"},
		{ID: "drv_2", VehicleID: "veh_a", UserID: "user_alpha"},
		{ID: "drv_3", VehicleID: "veh_b", UserID: "user_beta"},
		{ID: "drv_4", VehicleID: "veh_a", UserID: "user_alpha"},
	}
	insertErr := errors.New("insert failed")

	tests := []struct {
		name       string
		rows       []store.DriveBackfillRow
		failOn     int
		wantGroups int
		wantWrites int
		wantErr    error
	}{
		{
			name:       "one row per vehicle, not per drive",
			rows:       rows,
			wantGroups: 2,
			wantWrites: 2,
		},
		{
			name:       "no eligible drives writes nothing",
			rows:       nil,
			wantGroups: 0,
			wantWrites: 0,
		},
		{
			name:       "a failed insert aborts the run",
			rows:       rows,
			failOn:     1,
			wantGroups: 0,
			wantWrites: 1,
			wantErr:    insertErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &fakeDecryptRecorder{failOn: tt.failOn, failErr: insertErr}
			groups, err := auditGeocodeBackfill(context.Background(), rec, "jdoe", tt.rows)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if groups != tt.wantGroups {
				t.Errorf("groups = %d, want %d", groups, tt.wantGroups)
			}
			if len(rec.got) != tt.wantWrites {
				t.Fatalf("wrote %d audit row(s), want %d", len(rec.got), tt.wantWrites)
			}

			for _, a := range rec.got {
				if a.Command != "ops geocode backfill" {
					t.Errorf("Command = %q", a.Command)
				}
				if a.TargetType != store.OperatorTargetVehicle {
					t.Errorf("TargetType = %q, want the Vehicle target", a.TargetType)
				}
				// targetId is the Vehicle cuid, never the VIN (P0 column).
				if a.TargetID == "" || len(a.TargetID) == 17 {
					t.Errorf("TargetID = %q, want a Vehicle cuid and never a 17-char VIN", a.TargetID)
				}
				// Field NAMES only — store.validateFieldName rejects
				// anything value-shaped, so a drifted vocabulary here
				// would fail closed in production (CG-DL-5).
				if len(a.Fields) != len(geocodeBackfillAuditFields) {
					t.Errorf("Fields = %v, want %v", a.Fields, geocodeBackfillAuditFields)
				}
			}
		})
	}
}

// TestAuditGeocodeBackfill_SubjectPerVehicle pins that each group is keyed
// on ITS OWN owner. Getting this wrong would attribute one user's decrypt
// to another — the failure the fleet.go owner-mismatch check exists to
// prevent, reappearing in a fleet-wide command.
func TestAuditGeocodeBackfill_SubjectPerVehicle(t *testing.T) {
	rec := &fakeDecryptRecorder{}
	rows := []store.DriveBackfillRow{
		{ID: "drv_1", VehicleID: "veh_a", UserID: "user_alpha"},
		{ID: "drv_2", VehicleID: "veh_b", UserID: "user_beta"},
	}
	if _, err := auditGeocodeBackfill(context.Background(), rec, "jdoe", rows); err != nil {
		t.Fatalf("auditGeocodeBackfill: %v", err)
	}

	want := map[string]string{"veh_a": "user_alpha", "veh_b": "user_beta"}
	for _, a := range rec.got {
		if got := want[a.TargetID]; got != a.UserID {
			t.Errorf("vehicle %s audited against subject %q, want %q", a.TargetID, a.UserID, got)
		}
	}
}
