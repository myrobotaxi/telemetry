package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeEvidenceNotifier struct{ vins []string }

func (f *fakeEvidenceNotifier) PairingEvidence(vin string) { f.vins = append(f.vins, vin) }

// TestCompleteSetupHandsPairingToTheReconciler is MYR-529's §7.23 half.
//
// The endpoint already does the strongest thing it can on a paired answer —
// reset the schedule and spend the epoch's forced re-push — but it has THREE
// paired outcomes it cannot finish itself: the budget was already spent, the
// deployment has no signing proxy, and the car refused to wake. Each one used
// to leave `next_attempt_at` exactly where it stood. Tesla's paired answer is
// now handed to the reconcile loop on every one of them, which is what arms the
// hot ladder.
//
// The unpaired and token-failure answers must stay silent: neither is evidence
// of a key, and signalling one would open a pairing epoch that nothing proved.
func TestCompleteSetupHandsPairingToTheReconciler(t *testing.T) {
	tests := []struct {
		name       string
		row        VehicleSnapshotRow
		setup      func(*setupHarness)
		wantSignal bool
	}{
		{
			name:       "paired and configured",
			row:        setupRow(withEpoch(2*time.Hour, 0)),
			wantSignal: true,
		},
		{
			name:       "paired but the epoch budget was already spent",
			row:        setupRow(withEpoch(2*time.Hour, time.Hour)),
			wantSignal: true,
		},
		{
			name: "paired but this deployment cannot push config",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.repusher = nil
			},
			wantSignal: true,
		},
		{
			name: "paired but the car refused to wake",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.probe.wakeErr = errors.New("408 timeout")
			},
			wantSignal: true,
		},
		{
			name: "Tesla says the key is NOT enrolled",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.probe.answers = []probeAnswer{{paired: false}}
			},
			wantSignal: false,
		},
		{
			name: "the pairing probe never got an answer",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.probe.answers = []probeAnswer{{err: errors.New("429")}}
			},
			wantSignal: false,
		},
		{
			name: "the car is already streaming, so there is nothing to arm",
			row: setupRow(func(r *VehicleSnapshotRow) {
				r.Status = "parked"
				r.LastUpdated = setupNow.Add(-20 * time.Second)
			}),
			wantSignal: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSetupHarness([]VehicleSnapshotRow{tt.row}, tt.setup)
			notifier := &fakeEvidenceNotifier{}
			h.completer.deps.Notifier = notifier

			// The state/error is asserted exhaustively elsewhere; what is under
			// test here is purely whether the reconciler was told.
			_, _ = h.completer.Complete(context.Background(), tt.row)

			got := len(notifier.vins) > 0
			if got != tt.wantSignal {
				t.Fatalf("signalled = %v (%v), want %v", got, notifier.vins, tt.wantSignal)
			}
			if got && notifier.vins[0] != tt.row.VIN {
				t.Errorf("signalled VIN = %q, want %q", notifier.vins[0], tt.row.VIN)
			}
			if got && len(notifier.vins) != 1 {
				t.Errorf("signalled %d times, want exactly 1", len(notifier.vins))
			}
		})
	}
}

// TestCompleteSetupSignalsAfterTheForce pins the ordering that keeps the two
// paths from racing for one epoch's single escalation. Signalled BEFORE the
// force, the loop could reset and reconcile this vehicle while the sequence is
// still mid-push; signalled after, the loop's own reset falls inside the
// pairing debounce that configure just refreshed.
func TestCompleteSetupSignalsAfterTheForce(t *testing.T) {
	row := setupRow(withEpoch(2*time.Hour, 0))
	h := newSetupHarness([]VehicleSnapshotRow{row}, nil)
	notifier := &fakeEvidenceNotifier{}
	h.completer.deps.Notifier = notifier

	var pushedBeforeSignal bool
	h.repusher.onPush = func() { pushedBeforeSignal = len(notifier.vins) == 0 }

	if _, err := h.completer.Complete(context.Background(), row); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if h.repusher.count() != 1 {
		t.Fatalf("forced pushes = %d, want 1", h.repusher.count())
	}
	if !pushedBeforeSignal {
		t.Error("the reconciler was signalled before the forced re-push ran; the two would race for the epoch budget")
	}
}

// TestCompleteSetupWithoutANotifierIsUnchanged: a deployment with no reconciler
// has nobody to tell, and must not crash trying.
func TestCompleteSetupWithoutANotifierIsUnchanged(t *testing.T) {
	row := setupRow(withEpoch(2*time.Hour, 0))
	h := newSetupHarness([]VehicleSnapshotRow{row}, nil)

	state, err := h.completer.Complete(context.Background(), row)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if state.State != SetupStateConfiguring {
		t.Errorf("state = %q, want %q", state.State, SetupStateConfiguring)
	}
}
