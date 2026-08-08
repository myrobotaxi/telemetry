package config

import (
	"errors"
	"os"
	"testing"
)

// The MYR-447 ride-retention kill-switch (RIDE_RETENTION_PRUNER_ENABLED).
//
// Two properties matter and neither is obvious from the accessor: it defaults
// ON when unset, and it FAILS THE WHOLE LOAD on anything ParseBool rejects. The
// second is the one worth a test — an `off` or `no` typo silently reading as
// either value would leave an operator wrong about whether a privacy sweep is
// running, and "off" is the reading that quietly suspends it.

func TestLoad_RidePrunerEnabledParsing(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		value   string
		want    bool
		wantErr bool
	}{
		{name: "unset defaults to true", set: false, want: true},
		{name: "false disables", set: true, value: "false", want: false},
		{name: "0 disables", set: true, value: "0", want: false},
		{name: "False disables", set: true, value: "False", want: false},
		{name: "true enables", set: true, value: "true", want: true},
		{name: "1 enables", set: true, value: "1", want: true},
		{name: "TRUE enables", set: true, value: "TRUE", want: true},
		{name: "empty is invalid", set: true, value: "", wantErr: true},
		{name: "no is invalid", set: true, value: "no", wantErr: true},
		{name: "off is invalid", set: true, value: "off", wantErr: true},
		{name: "yes is invalid", set: true, value: "yes", wantErr: true},
		{name: "garbage is invalid", set: true, value: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestConfig(t, dir, nil)
			setRequiredEnv(t)
			if tt.set {
				t.Setenv("RIDE_RETENTION_PRUNER_ENABLED", tt.value)
			} else {
				os.Unsetenv("RIDE_RETENTION_PRUNER_ENABLED")
			}

			cfg, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() expected error for RIDE_RETENTION_PRUNER_ENABLED=%q, got nil", tt.value)
				}
				if !errors.Is(err, ErrInvalidValue) {
					t.Errorf("error should wrap ErrInvalidValue, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.RidePrunerEnabled() != tt.want {
				t.Errorf("RidePrunerEnabled() = %v, want %v", cfg.RidePrunerEnabled(), tt.want)
			}
		})
	}
}

// TestLoad_RideAndDrivePrunerSwitchesAreIndependent pins the deliberate
// separation: the two sweeps share an engine, and it would be an easy edit to
// have them share a switch too. Stopping a misbehaving drive sweep must not
// silently suspend ride retention, or vice versa.
func TestLoad_RideAndDrivePrunerSwitchesAreIndependent(t *testing.T) {
	tests := []struct {
		name                string
		driveEnv, rideEnv   string
		wantDrive, wantRide bool
	}{
		{name: "drive off leaves rides sweeping", driveEnv: "false", rideEnv: "true", wantDrive: false, wantRide: true},
		{name: "rides off leaves drives sweeping", driveEnv: "true", rideEnv: "false", wantDrive: true, wantRide: false},
		{name: "both off", driveEnv: "false", rideEnv: "false", wantDrive: false, wantRide: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestConfig(t, dir, nil)
			setRequiredEnv(t)
			t.Setenv("DRIVE_RETENTION_PRUNER_ENABLED", tt.driveEnv)
			t.Setenv("RIDE_RETENTION_PRUNER_ENABLED", tt.rideEnv)

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.DrivePrunerEnabled() != tt.wantDrive {
				t.Errorf("DrivePrunerEnabled() = %v, want %v", cfg.DrivePrunerEnabled(), tt.wantDrive)
			}
			if cfg.RidePrunerEnabled() != tt.wantRide {
				t.Errorf("RidePrunerEnabled() = %v, want %v", cfg.RidePrunerEnabled(), tt.wantRide)
			}
		})
	}
}
