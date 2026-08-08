package commands

import (
	"context"
	"errors"
	"testing"
)

// recordingObserver captures the VINs reported as applied.
type recordingObserver struct {
	vins []string
}

func (o *recordingObserver) SignedCommandApplied(vin string) { o.vins = append(o.vins, vin) }

const observerTestVIN = "7SAYGDED5TA736164"

// TestSignedCommandObserver pins the MYR-489 contract: the observer fires on a
// successfully applied SIGNED command and on nothing else. What it reports is
// used as proof that an owner's virtual key is enrolled, so a false positive
// would arm an escalation against a car that was never paired.
func TestSignedCommandObserver(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		results  []TransportResult
		cmdErr   error
		wantVINs int
	}{
		{
			name:     "applied signed command reports",
			command:  "door_lock",
			results:  []TransportResult{{Outcome: OutcomeOK}},
			wantVINs: 1,
		},
		{
			// navigation_request goes straight to the Fleet REST API and works
			// with no virtual key at all, so it proves nothing about pairing.
			name:     "applied UNSIGNED command does not report",
			command:  "navigation_request",
			results:  []TransportResult{{Outcome: OutcomeOK}},
			wantVINs: 0,
		},
		{
			name:     "not-paired refusal does not report",
			command:  "door_lock",
			results:  []TransportResult{{Outcome: OutcomeNotPaired}},
			wantVINs: 0,
		},
		{
			name:     "vehicle refusal does not report",
			command:  "door_lock",
			results:  []TransportResult{{Outcome: OutcomeFailed, Reason: "vehicle refused"}},
			wantVINs: 0,
		},
		{
			name:     "transport error does not report",
			command:  "door_lock",
			cmdErr:   errors.New("dial tcp: connection refused"),
			wantVINs: 0,
		},
		{
			// The car woke and then applied: still one report, not two.
			name:    "wake-then-apply reports exactly once",
			command: "door_lock",
			results: []TransportResult{
				{Outcome: OutcomeAsleep},
				{Outcome: OutcomeOK},
			},
			wantVINs: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := &recordingObserver{}
			transport := &fakeTransport{enabled: true, results: tt.results, cmdErr: tt.cmdErr}
			e := NewExecutor(transport, nil,
				WithConfig(fastConfig()),
				WithSignedCommandObserver(obs))

			params := map[string]any(nil)
			if tt.command == "navigation_request" {
				params = map[string]any{"value": "1 Main St"}
			}
			_, _ = e.Execute(context.Background(), Request{
				VIN:     observerTestVIN,
				Command: tt.command,
				Params:  params,
				Scopes:  grantAll(),
			})

			if len(obs.vins) != tt.wantVINs {
				t.Fatalf("observer calls = %d (%v), want %d", len(obs.vins), obs.vins, tt.wantVINs)
			}
			if tt.wantVINs > 0 && obs.vins[0] != observerTestVIN {
				t.Errorf("reported VIN = %q, want %q", obs.vins[0], observerTestVIN)
			}
		})
	}
}

// TestExecutorWithoutObserverStillApplies: every process that does not run the
// fleet-config reconciler leaves the hook nil, and must behave exactly as it
// did before MYR-489.
func TestExecutorWithoutObserverStillApplies(t *testing.T) {
	transport := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: OutcomeOK}}}
	e := NewExecutor(transport, nil, WithConfig(fastConfig()))

	res, err := e.Execute(context.Background(), Request{
		VIN: observerTestVIN, Command: "door_lock", Scopes: grantAll(),
	})
	if err != nil {
		t.Fatalf("Execute: unexpected error: %v", err)
	}
	if !res.Applied {
		t.Error("Applied = false, want true")
	}
}
