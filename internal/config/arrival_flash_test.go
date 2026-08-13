package config

import (
	"errors"
	"os"
	"testing"
)

// The MYR-542 arrival light-flash kill-switch (ARRIVAL_FLASH_ENABLED).
//
// Same two properties as every other switch in load_dispatch.go — ON when
// unset, and the whole load FAILS on anything ParseBool rejects. The fail-fast
// half is worth as much here as it is for auto-arrival, for a different reason:
// this switch is the only thing standing between a misfiring detector and a car
// flashing its headlights in a street at 3am, and an operator who typed
// `ARRIVAL_FLASH_ENABLED=off` must not be left believing they had stopped it.

func TestLoad_ArrivalFlashEnabledParsing(t *testing.T) {
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
		{name: "garbage is invalid", set: true, value: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestConfig(t, dir, nil)
			setRequiredEnv(t)
			if tt.set {
				t.Setenv("ARRIVAL_FLASH_ENABLED", tt.value)
			} else {
				os.Unsetenv("ARRIVAL_FLASH_ENABLED")
			}

			cfg, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() expected error for ARRIVAL_FLASH_ENABLED=%q, got nil", tt.value)
				}
				if !errors.Is(err, ErrInvalidValue) {
					t.Errorf("error should wrap ErrInvalidValue, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.ArrivalFlashEnabled() != tt.want {
				t.Errorf("ArrivalFlashEnabled() = %v, want %v", cfg.ArrivalFlashEnabled(), tt.want)
			}
		})
	}
}

// TestLoad_ArrivalFlashIsIndependentOfAutoArrival pins the one separation that
// is not obvious. The flash cannot fire without auto-arrival — it consumes the
// seam auto-arrival publishes — so it would be easy to fold the two into one
// switch. They are separate because their failures are stopped by different
// people: silencing headlights must not cost the operator the arrival detection
// that is working, and stopping the detector must silence the lights with it.
func TestLoad_ArrivalFlashIsIndependentOfAutoArrival(t *testing.T) {
	tests := []struct {
		name                   string
		arrival, flash         string
		wantArrival, wantFlash bool
	}{
		{
			name:    "flash off leaves the detector writing arrivals",
			arrival: "true", flash: "false",
			wantArrival: true, wantFlash: false,
		},
		{
			name:    "detector off leaves the flash switch on but starved of events",
			arrival: "false", flash: "true",
			wantArrival: false, wantFlash: true,
		},
		{
			name:    "both on is the shipped default",
			arrival: "true", flash: "true",
			wantArrival: true, wantFlash: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestConfig(t, dir, nil)
			setRequiredEnv(t)
			t.Setenv("AUTO_ARRIVAL_ENABLED", tt.arrival)
			t.Setenv("ARRIVAL_FLASH_ENABLED", tt.flash)

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if cfg.AutoArrivalEnabled() != tt.wantArrival {
				t.Errorf("AutoArrivalEnabled() = %v, want %v", cfg.AutoArrivalEnabled(), tt.wantArrival)
			}
			if cfg.ArrivalFlashEnabled() != tt.wantFlash {
				t.Errorf("ArrivalFlashEnabled() = %v, want %v", cfg.ArrivalFlashEnabled(), tt.wantFlash)
			}
		})
	}
}
