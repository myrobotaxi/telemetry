package config

import (
	"errors"
	"os"
	"testing"
	"time"
)

// TestLoad_ServiceRepollEnabledParsing mirrors TestLoad_DispatchEnabledParsing:
// the kill-switch defaults ON and FAILS FAST on anything strconv.ParseBool
// rejects. The invalid cases are the point — `no`, `off` and `yes` are exactly
// what an operator reaches for under pressure, and silently reading them as the
// default would leave someone believing they had stopped the Fleet API traffic
// when they had not (the MYR-176 lesson: a kill-switch you cannot trust is worse
// than none).
func TestLoad_ServiceRepollEnabledParsing(t *testing.T) {
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
		{name: "True enables", set: true, value: "True", want: true},
		{name: "1 enables", set: true, value: "1", want: true},
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
				t.Setenv("SERVICE_REPOLL_ENABLED", tt.value)
			} else {
				os.Unsetenv("SERVICE_REPOLL_ENABLED")
			}

			cfg, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() expected error for SERVICE_REPOLL_ENABLED=%q, got nil", tt.value)
				}
				if !errors.Is(err, ErrInvalidValue) {
					t.Errorf("error should wrap ErrInvalidValue, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.ServiceRepollEnabled() != tt.want {
				t.Errorf("ServiceRepollEnabled() = %v, want %v", cfg.ServiceRepollEnabled(), tt.want)
			}
		})
	}
}

// TestLoad_ServiceRepollIntervalParsing: the cadence is the only lever on the
// Fleet API request rate this feature adds, so a typo must surface at startup
// rather than hiding behind the default for however long it takes somebody to
// compare the logs against what they meant.
//
// A ZERO or NEGATIVE value is rejected outright rather than clamped. It is not a
// request for "as fast as possible" and it is not a request to disable — the
// kill-switch exists for that — so the only honest response is to refuse it.
func TestLoad_ServiceRepollIntervalParsing(t *testing.T) {
	tests := []struct {
		name    string
		set     bool
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset defaults to 15m", set: false, want: 15 * time.Minute},
		{name: "30m widens the cadence", set: true, value: "30m", want: 30 * time.Minute},
		{name: "90s narrows it", set: true, value: "90s", want: 90 * time.Second},
		{name: "1h30m compound duration", set: true, value: "1h30m", want: 90 * time.Minute},
		{name: "zero is invalid", set: true, value: "0s", wantErr: true},
		{name: "negative is invalid", set: true, value: "-5m", wantErr: true},
		{name: "empty is invalid", set: true, value: "", wantErr: true},
		{name: "bare number is invalid", set: true, value: "15", wantErr: true},
		{name: "garbage is invalid", set: true, value: "soon", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestConfig(t, dir, nil)
			setRequiredEnv(t)
			if tt.set {
				t.Setenv("SERVICE_REPOLL_INTERVAL", tt.value)
			} else {
				os.Unsetenv("SERVICE_REPOLL_INTERVAL")
			}

			cfg, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() expected error for SERVICE_REPOLL_INTERVAL=%q, got nil", tt.value)
				}
				if !errors.Is(err, ErrInvalidValue) {
					t.Errorf("error should wrap ErrInvalidValue, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.ServiceRepollInterval() != tt.want {
				t.Errorf("ServiceRepollInterval() = %v, want %v", cfg.ServiceRepollInterval(), tt.want)
			}
		})
	}
}
