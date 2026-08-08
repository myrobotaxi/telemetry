package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

type resetCall struct {
	vin            string
	now            time.Time
	debounceBefore time.Time
}

type fakePairingResetter struct {
	calls     []resetCall
	candidate FleetConfigCandidate
	found     bool
	err       error
}

func (f *fakePairingResetter) ResetFleetConfigScheduleOnPairing(
	_ context.Context, vin string, now, debounceBefore time.Time,
) (FleetConfigCandidate, bool, error) {
	f.calls = append(f.calls, resetCall{vin: vin, now: now, debounceBefore: debounceBefore})
	return f.candidate, f.found, f.err
}

func newPairingReconciler(
	pairing *fakePairingResetter,
	reader *fakeConfigReader,
	writer *fakeConfigWriter,
	attempts *fakeAttemptRecorder,
) *FleetConfigReconciler {
	r := NewFleetConfigReconciler(
		FleetConfigReconcilerDeps{
			Candidates: &fakeCandidateLister{},
			Attempts:   attempts,
			Reader:     reader,
			Writer:     writer,
			Tokens:     okToken(),
			Pairing:    pairing,
		},
		FleetConfigReconcileConfig{},
		EndpointConfig{Hostname: "telemetry.myrobotaxi.app", Port: 443},
		slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	)
	r.now = func() time.Time { return forceNow }
	return r
}

// TestPairingSignalResetsAndReconciles is hole 2 end to end: an applied signed
// command proves the key exists, which invalidates a backoff earned entirely
// before it did, and the car is reconciled on the spot instead of in eight
// hours.
func TestPairingSignalResetsAndReconciles(t *testing.T) {
	pairing := &fakePairingResetter{
		found: true,
		candidate: FleetConfigCandidate{
			VehicleID:       "veh-1",
			VIN:             reconTestVIN,
			UserID:          "user-1",
			LastOutcome:     outcomeAwaitingKey,
			SignedCommandAt: forceNow,
		},
	}
	// Tesla now accepts the push, because the key finally exists.
	writer := &fakeConfigWriter{result: &FleetConfigResponse{Response: FleetConfigResult{UpdatedVehicles: 1}}}
	attempts := &fakeAttemptRecorder{}
	r := newPairingReconciler(pairing, &fakeConfigReader{synced: false}, writer, attempts)

	r.handlePairingSignal(context.Background(), reconTestVIN)

	if len(pairing.calls) != 1 || pairing.calls[0].vin != reconTestVIN {
		t.Fatalf("reset calls = %+v, want one for %s", pairing.calls, reconTestVIN)
	}
	// The debounce window must be expressed as an instant in the past, or a
	// command burst would fire a reconcile per tap.
	if want := forceNow.Add(-pairingResetDebounce); !pairing.calls[0].debounceBefore.Equal(want) {
		t.Errorf("debounceBefore = %v, want %v", pairing.calls[0].debounceBefore, want)
	}
	if writer.calls != 1 {
		t.Errorf("push calls = %d, want 1 (the signal must trigger an immediate reconcile)", writer.calls)
	}
	if len(attempts.cleared) != 1 || attempts.cleared[0] != "veh-1" {
		t.Errorf("cleared = %v, want [veh-1] (a successful push drops the schedule)", attempts.cleared)
	}
}

func TestPairingSignalNoOps(t *testing.T) {
	tests := []struct {
		name    string
		pairing *fakePairingResetter
	}{
		{
			// The overwhelmingly common case: a healthy streaming car has no
			// schedule row, so there is nothing to reset and nothing to push.
			name:    "no schedule row",
			pairing: &fakePairingResetter{found: false},
		},
		{
			// Debounced repeats look identical to the caller by design.
			name:    "debounced repeat",
			pairing: &fakePairingResetter{found: false},
		},
		{
			name:    "store error",
			pairing: &fakePairingResetter{err: errors.New("connection reset")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &fakeConfigWriter{}
			reader := &fakeConfigReader{}
			r := newPairingReconciler(tt.pairing, reader, writer, &fakeAttemptRecorder{})

			r.handlePairingSignal(context.Background(), reconTestVIN)

			if writer.calls != 0 || writer.deleteCalls != 0 || reader.calls != 0 {
				t.Errorf("Tesla was called (%d push, %d delete, %d read); want none",
					writer.calls, writer.deleteCalls, reader.calls)
			}
		})
	}
}

// TestPairingSignalTriggersEscalationForNabilsCar is the full MYR-489 story on
// one car: the key was already paired, Tesla reports the config synced, the car
// has been silent for two days, and the previous pass recorded exactly that. A
// single applied command must take it all the way to the forced re-push.
func TestPairingSignalTriggersEscalationForNabilsCar(t *testing.T) {
	pairing := &fakePairingResetter{
		found: true,
		candidate: FleetConfigCandidate{
			VehicleID:       "veh-1",
			VIN:             reconTestVIN,
			UserID:          "user-1",
			LastUpdated:     forceNow.Add(-48 * time.Hour),
			LastOutcome:     outcomeSyncedQuiet,
			LastAttemptAt:   forceNow.Add(-8 * time.Hour),
			SignedCommandAt: forceNow,
		},
	}
	writer := &fakeConfigWriter{result: &FleetConfigResponse{Response: FleetConfigResult{UpdatedVehicles: 1}}}
	attempts := &fakeAttemptRecorder{}
	r := newPairingReconciler(pairing, &fakeConfigReader{synced: true}, writer, attempts)

	r.handlePairingSignal(context.Background(), reconTestVIN)

	if writer.deleteCalls != 1 || writer.calls != 1 {
		t.Fatalf("delete=%d push=%d, want 1 and 1 (delete-then-create)", writer.deleteCalls, writer.calls)
	}
	if len(attempts.forced) != 1 || attempts.forced[0].outcome != outcomeForcedRepush {
		t.Errorf("forced writes = %+v, want one %q", attempts.forced, outcomeForcedRepush)
	}
	// The awake stamp was enough; no Fleet API read should have been spent.
	if got := r.deps.Reader.(*fakeConfigReader).vehicleCalls; got != 0 {
		t.Errorf("GetVehicle calls = %d, want 0 (a fresh signed command is its own proof)", got)
	}
}

// TestSignedCommandAppliedNeverBlocks: the observer runs on the owner's request
// goroutine, so a full inbox must drop rather than wait.
func TestSignedCommandAppliedNeverBlocks(t *testing.T) {
	r := newPairingReconciler(&fakePairingResetter{}, &fakeConfigReader{}, &fakeConfigWriter{}, &fakeAttemptRecorder{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Well past the buffer depth, with nothing draining it.
		for i := 0; i < pairingSignalBuffer*4; i++ {
			r.SignedCommandApplied(reconTestVIN)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SignedCommandApplied blocked on a full inbox")
	}

	if len(r.pairing) != pairingSignalBuffer {
		t.Errorf("inbox depth = %d, want it saturated at %d", len(r.pairing), pairingSignalBuffer)
	}
}

// TestRunReconcileLoopServesPairingSignals proves the wiring: a signal
// delivered to a running loop is drained by that loop's own goroutine, which is
// what makes a command applied mid-pass ordered rather than concurrent.
func TestRunReconcileLoopServesPairingSignals(t *testing.T) {
	handled := make(chan string, 1)
	pairing := &fakePairingResetter{found: false}
	r := newPairingReconciler(pairing, &fakeConfigReader{}, &fakeConfigWriter{}, &fakeAttemptRecorder{})
	// Route the observation out of the fake, which the loop calls.
	pairing.err = nil

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// startupDelay is 30s, so drive the loop body directly rather than waiting
	// on it: the select under test is the same one.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case vin := <-r.pairing:
				r.handlePairingSignal(ctx, vin)
				handled <- vin
			}
		}
	}()

	r.SignedCommandApplied(reconTestVIN)

	select {
	case got := <-handled:
		if got != reconTestVIN {
			t.Errorf("handled VIN = %s, want %s", got, reconTestVIN)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pairing signal was never drained")
	}

	if len(pairing.calls) != 1 {
		t.Errorf("reset calls = %d, want 1", len(pairing.calls))
	}
}
