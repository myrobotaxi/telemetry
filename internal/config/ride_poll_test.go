package config

// MYR-394 ride-position poll config tests.

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLoad_RidePollEnabledParsing mirrors the dispatch and re-poll switches:
// defaults ON, fails fast on anything ParseBool rejects. `no` and `off` are
// exactly what an operator reaches for under pressure, and reading them as the
// default would leave someone believing they had stopped the Fleet API traffic
// when they had not.
func TestLoad_RidePollEnabledParsing(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		value   string
		want    bool
		wantErr bool
	}{
		{name: "unset defaults to true", want: true},
		{name: "false disables", set: true, value: "false"},
		{name: "0 disables", set: true, value: "0"},
		{name: "true enables", set: true, value: "true", want: true},
		{name: "empty is invalid", set: true, value: "", wantErr: true},
		{name: "no is invalid", set: true, value: "no", wantErr: true},
		{name: "off is invalid", set: true, value: "off", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestConfig(t, t.TempDir(), nil)
			setRequiredEnv(t)
			if tt.set {
				t.Setenv("RIDE_POSITION_POLL_ENABLED", tt.value)
			} else {
				os.Unsetenv("RIDE_POSITION_POLL_ENABLED")
			}

			cfg, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() expected error for RIDE_POSITION_POLL_ENABLED=%q, got nil", tt.value)
				}
				if !errors.Is(err, ErrInvalidValue) {
					t.Errorf("error should wrap ErrInvalidValue, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if got := cfg.RidePollEnabled(); got != tt.want {
				t.Errorf("RidePollEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLoad_RidePollIntervalValidation is the rate-budget guard. This knob is
// RANGE-checked, not merely positive-checked, and both ends matter:
//
//   - `1s` is a plausible typo for `1m`, and it is the kind that gets an app's
//     Fleet API access rate-limited before anyone notices.
//   - anything past five minutes produces a marker stale enough that showing it
//     as the car's position is arguably a worse lie than the defect MYR-394
//     exists to fix.
//
// Both must be startup failures, so a bad value is caught by the operator who
// typed it rather than by Tesla.
func TestLoad_RidePollIntervalValidation(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset defaults to 25s", want: 25 * time.Second},
		{name: "20s accepted", set: true, value: "20s", want: 20 * time.Second},
		{name: "30s accepted", set: true, value: "30s", want: 30 * time.Second},
		{name: "floor accepted", set: true, value: "10s", want: 10 * time.Second},
		{name: "ceiling accepted", set: true, value: "5m", want: 5 * time.Minute},

		{name: "1s is below the floor", set: true, value: "1s", wantErr: true},
		{name: "9s is below the floor", set: true, value: "9s", wantErr: true},
		{name: "10m is above the ceiling", set: true, value: "10m", wantErr: true},
		{name: "zero is rejected", set: true, value: "0s", wantErr: true},
		{name: "negative is rejected", set: true, value: "-30s", wantErr: true},
		{name: "not a duration", set: true, value: "25", wantErr: true},
		{name: "garbage", set: true, value: "soon", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestConfig(t, t.TempDir(), nil)
			setRequiredEnv(t)
			if tt.set {
				t.Setenv("RIDE_POSITION_POLL_INTERVAL", tt.value)
			} else {
				os.Unsetenv("RIDE_POSITION_POLL_INTERVAL")
			}

			cfg, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() expected error for RIDE_POSITION_POLL_INTERVAL=%q, got nil", tt.value)
				}
				if !errors.Is(err, ErrInvalidValue) {
					t.Errorf("error should wrap ErrInvalidValue, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if got := cfg.RidePollInterval(); got != tt.want {
				t.Errorf("RidePollInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLoad_RidePollIntervalErrorNamesItsKillSwitch — parseDurationEnv had the
// service-repoll switch hard-coded into this message and now has two callers,
// so an operator who types 0 must be told about the RIGHT switch.
func TestLoad_RidePollIntervalErrorNamesItsKillSwitch(t *testing.T) {
	path := writeTestConfig(t, t.TempDir(), nil)
	setRequiredEnv(t)
	t.Setenv("RIDE_POSITION_POLL_INTERVAL", "0s")

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected an error for a zero interval")
	}
	if got := err.Error(); !strings.Contains(got, "RIDE_POSITION_POLL_ENABLED") {
		t.Errorf("error %q should name RIDE_POSITION_POLL_ENABLED as the way to disable the loop", got)
	}
}
