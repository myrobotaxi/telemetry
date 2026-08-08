package telemetry

import (
	"errors"
	"strings"
	"testing"
)

// The regression this file exists for: Tesla answers a fleet-config push for
// an unpaired car with HTTP 200 and updated_vehicles: 0. Treating that as
// success is what left every external beta owner never streaming (MYR-448).
func TestSkipErrorFor(t *testing.T) {
	const vin = "7SAYGDED5TA736164"

	tests := []struct {
		name       string
		result     *FleetConfigResponse
		wantSkip   bool
		wantReason string
		wantAwait  bool
	}{
		{
			name:     "nil result yields no skip error",
			result:   nil,
			wantSkip: false,
		},
		{
			name: "config applied to the vin",
			result: &FleetConfigResponse{Response: FleetConfigResult{
				UpdatedVehicles: 1,
			}},
			wantSkip: false,
		},
		{
			name: "another vin skipped, ours applied",
			result: &FleetConfigResponse{Response: FleetConfigResult{
				UpdatedVehicles: 1,
				SkippedVehicles: map[string]string{"5YJ3E1EA7JF000001": SkipReasonMissingKey},
			}},
			wantSkip: false,
		},
		{
			name: "missing_key is a skip and is recoverable",
			result: &FleetConfigResponse{Response: FleetConfigResult{
				UpdatedVehicles: 0,
				SkippedVehicles: map[string]string{vin: SkipReasonMissingKey},
			}},
			wantSkip:   true,
			wantReason: SkipReasonMissingKey,
			wantAwait:  true,
		},
		{
			name: "not_paired is a skip and is recoverable",
			result: &FleetConfigResponse{Response: FleetConfigResult{
				SkippedVehicles: map[string]string{vin: SkipReasonNotPaired},
			}},
			wantSkip:   true,
			wantReason: SkipReasonNotPaired,
			wantAwait:  true,
		},
		{
			name: "an unrecognised reason is a skip but not a pairing wait",
			result: &FleetConfigResponse{Response: FleetConfigResult{
				SkippedVehicles: map[string]string{vin: "unsupported_hardware"},
			}},
			wantSkip:   true,
			wantReason: "unsupported_hardware",
			wantAwait:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := SkipErrorFor(tc.result, vin)

			if !tc.wantSkip {
				if err != nil {
					t.Fatalf("SkipErrorFor() = %v, want nil", err)
				}
				return
			}

			if err == nil {
				t.Fatal("SkipErrorFor() = nil, want a skip error")
			}
			if !errors.Is(err, ErrVehicleSkipped) {
				t.Errorf("errors.Is(err, ErrVehicleSkipped) = false, want true")
			}

			var skipped *SkippedVehicleError
			if !errors.As(err, &skipped) {
				t.Fatalf("errors.As(*SkippedVehicleError) = false for %T", err)
			}
			if skipped.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", skipped.Reason, tc.wantReason)
			}
			if got := skipped.AwaitingVirtualKey(); got != tc.wantAwait {
				t.Errorf("AwaitingVirtualKey() = %v, want %v", got, tc.wantAwait)
			}

			// P0 log-safety: the full VIN must never reach the error string.
			if strings.Contains(err.Error(), vin) {
				t.Errorf("error string leaks the full VIN: %q", err.Error())
			}
			if !strings.Contains(err.Error(), "6164") {
				t.Errorf("error string should carry the redacted last-4, got %q", err.Error())
			}
		})
	}
}
