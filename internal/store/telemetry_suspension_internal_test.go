package store

import (
	"strings"
	"testing"
)

// MYR-592 — the structural guard behind fleetConfigAbsentOutcomes.
//
// The predicate is a SQL literal because it is embedded in a `const` statement,
// so nothing in the compiler holds it to the Go-side SetupOutcome* constants it
// is meant to mirror. A rename on either side would be silent: the sweeper would
// simply stop excluding (or start excluding) a class of cars, and the only
// symptom would be a Tesla bill that did not fall, or warnings sent about cars
// that never streamed. This test is the join.
func TestFleetConfigAbsentOutcomesCoverSetupLabels(t *testing.T) {
	tests := []struct {
		name    string
		label   string
		wantIn  bool
		because string
	}{
		{
			name:    "awaiting_virtual_key is excluded",
			label:   SetupOutcomeAwaitingVirtualKey,
			wantIn:  true,
			because: "Tesla refused to install the config, so nothing is streaming and nothing is billed",
		},
		{
			name:    "push_failed is excluded",
			label:   SetupOutcomePushFailed,
			wantIn:  true,
			because: "the push never landed, so there is no config to remove",
		},
		{
			name:   "the empty outcome is NOT excluded",
			label:  SetupOutcomeNone,
			wantIn: false,
			because: "the link-time hook seeds it when the push APPLIED; excluding it would " +
				"exempt the healthiest cars in the fleet and disable the feature for most of it",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The empty label can never be "in" a quoted list by substring, so
			// test it the way the SQL does: as a whole quoted token.
			token := "'" + tc.label + "'"
			got := strings.Contains(fleetConfigAbsentOutcomes, token)
			if got != tc.wantIn {
				t.Errorf("fleetConfigAbsentOutcomes contains %s = %v, want %v (%s)",
					token, got, tc.wantIn, tc.because)
			}
		})
	}
}

// TestFleetConfigAbsentOutcomesIsAWellFormedList keeps the constant usable as a
// drop-in SQL fragment: it is concatenated into a query string, so a missing
// parenthesis is a runtime syntax error on the sweeper's candidate read rather
// than a compile failure.
func TestFleetConfigAbsentOutcomesIsAWellFormedList(t *testing.T) {
	if !strings.HasPrefix(fleetConfigAbsentOutcomes, "(") || !strings.HasSuffix(fleetConfigAbsentOutcomes, ")") {
		t.Fatalf("fleetConfigAbsentOutcomes = %q, want a parenthesised SQL list", fleetConfigAbsentOutcomes)
	}
	if !strings.Contains(queryInactiveOwnerVehicles, fleetConfigAbsentOutcomes) {
		t.Error("the candidate query no longer embeds fleetConfigAbsentOutcomes — " +
			"a second spelling of the label set is exactly what this constant exists to prevent")
	}
}
