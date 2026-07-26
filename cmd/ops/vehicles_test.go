package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestRunVehiclesReadd_FlagValidation pins the `ops vehicles re-add` argument
// contract (MYR-262) without touching a database: both --user-id and
// --tesla-vehicle-id are required, and they are validated BEFORE any DB
// connection is attempted (so a missing flag never opens a pool).
func TestRunVehiclesReadd_FlagValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "missing both flags errors on user-id first",
			args:    nil,
			wantErr: "--user-id is required",
		},
		{
			name:    "missing tesla-vehicle-id errors",
			args:    []string{"--user-id", "cuser1"},
			wantErr: "--tesla-vehicle-id is required",
		},
		{
			name:    "unknown flag errors from the flag set",
			args:    []string{"--nope", "x"},
			wantErr: "flag provided but not defined",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runVehiclesReadd(context.Background(), tt.args)
			if err == nil {
				t.Fatalf("runVehiclesReadd(%v) = nil, want error %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestRunVehicles_Dispatch verifies the `re-add` subcommand is routed and that
// an unknown subcommand is rejected.
func TestRunVehicles_Dispatch(t *testing.T) {
	// Unknown subcommand.
	if err := runVehicles(context.Background(), []string{"bogus"}); err == nil ||
		!strings.Contains(err.Error(), "unknown vehicles subcommand") {
		t.Errorf("runVehicles(bogus) error = %v, want unknown subcommand", err)
	}
	// re-add with no flags routes to runVehiclesReadd and fails flag validation
	// (proving it is dispatched, not rejected as unknown).
	if err := runVehicles(context.Background(), []string{"re-add"}); err == nil ||
		!strings.Contains(err.Error(), "--user-id is required") {
		t.Errorf("runVehicles(re-add) error = %v, want flag-validation error", err)
	}
}

// TestVehicleReaddOutput_JSONShape pins the ops re-add JSON wire shape.
func TestVehicleReaddOutput_JSONShape(t *testing.T) {
	b, err := json.Marshal(vehicleReaddOutput{
		UserID:         "cuser1",
		TeslaVehicleID: "vid-9",
		Cleared:        true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"userId":"cuser1"`, `"teslaVehicleId":"vid-9"`, `"cleared":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("json = %s, want it to contain %s", got, want)
		}
	}
}
