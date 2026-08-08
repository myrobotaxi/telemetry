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

// H4: the question is "did the config APPLY", not "is my exact string a map
// key". Getting that wrong recreates the MYR-448 bug one layer down — a
// "repaired" log line next to updated_vehicles: 0.
func TestSkipErrorForDoesNotTrustExactKeyMatching(t *testing.T) {
	const vin = "7SAYGDED5TA736164"

	tests := []struct {
		name       string
		result     *FleetConfigResponse
		wantSkip   bool
		wantReason string
	}{
		{
			name: "lowercase key from Tesla is still our vin",
			result: &FleetConfigResponse{Response: FleetConfigResult{
				UpdatedVehicles: 0,
				SkippedVehicles: map[string]string{"7saygded5ta736164": SkipReasonMissingKey},
			}},
			wantSkip:   true,
			wantReason: SkipReasonMissingKey,
		},
		{
			name: "padded key from Tesla is still our vin",
			result: &FleetConfigResponse{Response: FleetConfigResult{
				SkippedVehicles: map[string]string{" " + vin + " ": SkipReasonNotPaired},
			}},
			wantSkip:   true,
			wantReason: SkipReasonNotPaired,
		},
		{
			name: "updated nothing and itemised nothing is still not applied",
			result: &FleetConfigResponse{Response: FleetConfigResult{
				UpdatedVehicles: 0,
				SkippedVehicles: map[string]string{},
			}},
			wantSkip:   true,
			wantReason: SkipReasonNotApplied,
		},
		{
			name: "updated our car, unrelated vin skipped",
			result: &FleetConfigResponse{Response: FleetConfigResult{
				UpdatedVehicles: 1,
				SkippedVehicles: map[string]string{"5YJ3E1EA7JF000001": SkipReasonMissingKey},
			}},
			wantSkip: false,
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
			var skipped *SkippedVehicleError
			if !errors.As(err, &skipped) {
				t.Fatalf("SkipErrorFor() = %v, want a *SkippedVehicleError", err)
			}
			if skipped.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", skipped.Reason, tc.wantReason)
			}
		})
	}
}

// M2: Tesla error bodies echo the VIN, and the reconciler reprints them on a
// loop. Production logs are last-4 only.
func TestRedactedErrorTextScrubsVINs(t *testing.T) {
	const vin = "7SAYGDED5TA736164"

	err := &FleetAPIError{
		StatusCode: 400,
		Body:       `{"error":"vehicle ` + vin + ` is not configured","vins":["` + vin + `"]}`,
	}
	got := redactedErrorText(err)

	if strings.Contains(got, vin) {
		t.Errorf("full VIN survived redaction: %q", got)
	}
	if !strings.Contains(got, "6164") {
		t.Errorf("redacted last-4 missing, got %q", got)
	}
	if !strings.Contains(got, "400") {
		t.Errorf("status code should survive for diagnosis, got %q", got)
	}
	if redactedErrorText(nil) != "" {
		t.Error("redactedErrorText(nil) should be empty")
	}
}

func TestRedactedErrorTextTruncates(t *testing.T) {
	long := strings.Repeat("x", maxLoggedErrorText*3)
	got := redactedErrorText(&FleetAPIError{StatusCode: 500, Body: long})
	if len(got) > maxLoggedErrorText+32 {
		t.Errorf("len = %d, want capped near %d", len(got), maxLoggedErrorText)
	}
}
