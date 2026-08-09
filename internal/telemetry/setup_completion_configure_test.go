package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// withEpoch returns a row whose schedule row already carries a pairing epoch
// and, optionally, a forced re-push stamped against it.
func withEpoch(signedAgo, forcedAgo time.Duration) func(*VehicleSnapshotRow) {
	return func(r *VehicleSnapshotRow) {
		r.SetupSchedule.LastOutcome = outcomeSyncedQuiet
		if signedAgo > 0 {
			r.SetupSchedule.SignedCommandAt = setupNow.Add(-signedAgo)
		}
		if forcedAgo > 0 {
			r.SetupSchedule.ForcedRepushAt = setupNow.Add(-forcedAgo)
		}
	}
}

// TestCompleteSetupEpochBudget is the anti-hammering proof. A client looping
// complete-setup must not be able to loop delete-then-create against a real
// car: MYR-489's per-pairing-epoch budget is the bound, and MYR-505 extends it
// to this path rather than restating it.
func TestCompleteSetupEpochBudget(t *testing.T) {
	tests := []struct {
		name string
		// mutate shapes the schedule row's epoch columns.
		mutate     func(*VehicleSnapshotRow)
		wantPushes int
		wantState  string
	}{
		{
			// Never escalated: the budget is unspent and this call spends it.
			name:       "unspent epoch pushes once",
			mutate:     withEpoch(2*time.Hour, 0),
			wantPushes: 1,
			wantState:  SetupStateConfiguring,
		},
		{
			// The force came AFTER the epoch opened: spent. Reporting
			// `configuring` is still correct — the push happened, the car is
			// still being configured — it simply must not happen AGAIN.
			name:       "spent epoch reports configuring without pushing again",
			mutate:     withEpoch(2*time.Hour, time.Hour),
			wantPushes: 0,
			wantState:  SetupStateConfiguring,
		},
		{
			// A NEW epoch opened after the last force (the owner re-paired), so
			// the budget is re-armed. This is why the bound is a timestamp
			// comparison and not a boolean.
			name:       "a newer epoch re-arms the budget",
			mutate:     withEpoch(time.Hour, 2*time.Hour),
			wantPushes: 1,
			wantState:  SetupStateConfiguring,
		},
		{
			// No schedule row at all — the table is self-draining, so this is a
			// car nothing has examined. Delete-then-create degrades to create
			// (a 404 on the delete is tolerated), and the row the push writes
			// is what spends the budget for next time.
			name: "a car with no schedule row is still configured",
			mutate: func(r *VehicleSnapshotRow) {
				r.SetupSchedule = VehicleSetupSchedule{}
			},
			wantPushes: 1,
			wantState:  SetupStateConfiguring,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := setupRow(tt.mutate)
			h := newSetupHarness([]VehicleSnapshotRow{row}, nil)

			state, err := h.completer.Complete(context.Background(), row)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if state.State != tt.wantState {
				t.Errorf("state = %q, want %q", state.State, tt.wantState)
			}
			if got := h.repusher.count(); got != tt.wantPushes {
				t.Errorf("forced pushes = %d, want %d", got, tt.wantPushes)
			}
		})
	}
}

// TestCompleteSetupRepeatedCallsSpendOneEpoch drives the actual attack: a stuck
// client calling over and over while the car's schedule row moves exactly as
// production would move it. The second call must find the budget spent.
func TestCompleteSetupRepeatedCallsSpendOneEpoch(t *testing.T) {
	before := setupRow(withEpoch(2*time.Hour, 0))
	// After the first call's push, RecordForcedFleetConfigRepush has stamped
	// forced_repush_at at (or after) the epoch — this is that row.
	after := setupRow(withEpoch(2*time.Hour, 0))
	after.SetupSchedule.ForcedRepushAt = setupNow

	h := newSetupHarness([]VehicleSnapshotRow{before, after}, nil)

	for i := 1; i <= 3; i++ {
		state, err := h.completer.Complete(context.Background(), before)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if state.State != SetupStateConfiguring {
			t.Fatalf("call %d: state = %q, want %q", i, state.State, SetupStateConfiguring)
		}
	}

	if got := h.repusher.count(); got != 1 {
		t.Errorf("forced pushes across 3 calls = %d, want exactly 1 (one per pairing epoch)", got)
	}
}

// TestCompleteSetupLosesTheRaceToTheReconciler: the epoch is read from a row
// fetched AFTER the probe, not from the one the request started with. Without
// that re-read, a reconciler pass that escalated this very car during our
// 30-second probe budget would be double-spent.
func TestCompleteSetupLosesTheRaceToTheReconciler(t *testing.T) {
	// The row the request began with: budget unspent.
	before := setupRow(withEpoch(2*time.Hour, 0))
	// What the re-read finds: the background reconciler got there first, during
	// our probe budget. This is the ONLY GetByID the sequence performs, which is
	// exactly the point — the decision is made on post-probe truth.
	raced := setupRow(withEpoch(2*time.Hour, 0))
	raced.SetupSchedule.ForcedRepushAt = setupNow.Add(-time.Second)

	h := newSetupHarness([]VehicleSnapshotRow{raced}, nil)

	state, err := h.completer.Complete(context.Background(), before)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := h.repusher.count(); got != 0 {
		t.Fatalf("forced pushes = %d, want 0 — the reconciler already spent this epoch", got)
	}
	if state.State != SetupStateConfiguring {
		t.Errorf("state = %q, want %q (the reconciler's push is still real progress)", state.State, SetupStateConfiguring)
	}
	if state.Since != raced.SetupSchedule.ForcedRepushAt.UTC().Format(time.RFC3339) {
		t.Errorf("since = %q, want the reconciler's stamp %v", state.Since, raced.SetupSchedule.ForcedRepushAt)
	}
}

// TestCompleteSetupResetsTheSchedule: an on-demand completion must clear the
// backoff earned before the key existed, via the SAME write an applied signed
// command uses — including its debounce window, or a hammering client would
// slide the epoch forward on every call and re-arm the budget each time.
func TestCompleteSetupResetsTheSchedule(t *testing.T) {
	row := setupRow(nil)
	fresh := FleetConfigCandidate{
		VehicleID:       "veh-1",
		VIN:             reconTestVIN,
		UserID:          "user-1",
		LastOutcome:     outcomeAwaitingKey,
		SignedCommandAt: setupNow,
	}
	h := newSetupHarness([]VehicleSnapshotRow{row}, func(h *setupHarness) {
		h.pairing.candidate = fresh
		h.pairing.found = true
	})

	if _, err := h.completer.Complete(context.Background(), row); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if len(h.pairing.calls) != 1 {
		t.Fatalf("schedule resets = %d, want 1", len(h.pairing.calls))
	}
	call := h.pairing.calls[0]
	if call.vin != reconTestVIN {
		t.Errorf("reset VIN = %s, want %s", call.vin, reconTestVIN)
	}
	if want := setupNow.Add(-pairingResetDebounce); !call.debounceBefore.Equal(want) {
		t.Errorf("debounceBefore = %v, want %v (the same window MYR-489 uses)", call.debounceBefore, want)
	}
	// The push must receive the RESET candidate, not the pre-reset one: its
	// attempt_count is zero, which is what the next backoff is computed from.
	if len(h.repusher.got) != 1 {
		t.Fatalf("pushes = %d, want 1", len(h.repusher.got))
	}
	if got := h.repusher.got[0]; !got.SignedCommandAt.Equal(setupNow) || got.AttemptCount != 0 {
		t.Errorf("pushed candidate = %+v, want the reset one (epoch %v, attempt 0)", got, setupNow)
	}
}

// TestCompleteSetupToleratesAMissedReset: a debounced or absent reset is normal
// (the table is self-draining, and a signed command may have opened this epoch
// minutes ago) and must not stop the push.
func TestCompleteSetupToleratesAMissedReset(t *testing.T) {
	for _, tc := range []struct {
		name  string
		found bool
		err   error
	}{
		{name: "debounced or no row", found: false},
		{name: "database error", err: errors.New("pool exhausted")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := setupRow(withEpoch(2*time.Hour, 0))
			h := newSetupHarness([]VehicleSnapshotRow{row}, func(h *setupHarness) {
				h.pairing.found = tc.found
				h.pairing.err = tc.err
			})

			state, err := h.completer.Complete(context.Background(), row)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if state.State != SetupStateConfiguring {
				t.Errorf("state = %q, want %q", state.State, SetupStateConfiguring)
			}
			if got := h.repusher.count(); got != 1 {
				t.Errorf("forced pushes = %d, want 1 (a missed reset must not block the push)", got)
			}
		})
	}
}

// TestCompleteSetupStampsTheEpochForACarWithNoScheduleRow is MYR-517's latch at
// this layer. A vehicle with no schedule row used to get NO epoch stamp from
// the reset (it was an UPDATE), and then the forced re-push wrote
// forced_repush_at against a NULL signed_command_at — which reads back as
// `epochForceSpent` forever, so this endpoint could fire exactly once per car
// and then answered `configuring` from a spend that may have accomplished
// nothing. The store write now creates the row, so the candidate that reaches
// the push carries a live epoch and the budget re-arms as designed.
//
// The contrast with handlePairingSignal is deliberate: a created row makes the
// OBSERVER stand down (a car it has never recorded is not evidence of a broken
// one), and makes this endpoint proceed (the owner asked us to finish setting
// up this car, and Tesla just confirmed the key is enrolled).
func TestCompleteSetupStampsTheEpochForACarWithNoScheduleRow(t *testing.T) {
	// No schedule row at all: Present false, every stamp zero — Spencer White's
	// car, and every car whose link-time push did not answer `missing_key`.
	row := setupRow(func(r *VehicleSnapshotRow) {
		r.SetupSchedule = VehicleSetupSchedule{}
	})
	h := newSetupHarness([]VehicleSnapshotRow{row}, func(h *setupHarness) {
		h.pairing.found = true
		h.pairing.candidate = FleetConfigCandidate{
			VehicleID:       row.ID,
			VIN:             row.VIN,
			UserID:          row.UserID,
			SignedCommandAt: setupNow,
			ScheduleCreated: true,
		}
	})

	state, err := h.completer.Complete(context.Background(), row)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if state.State != SetupStateConfiguring {
		t.Errorf("state = %q, want %q", state.State, SetupStateConfiguring)
	}
	if len(h.repusher.got) != 1 {
		t.Fatalf("forced pushes = %d, want 1", len(h.repusher.got))
	}
	// The pushed candidate MUST carry the epoch, or forced_repush_at lands
	// against a NULL signed_command_at and latches the budget shut.
	if got := h.repusher.got[0]; !got.SignedCommandAt.Equal(setupNow) {
		t.Errorf("pushed candidate epoch = %v, want %v — an unstamped epoch latches the escalation budget",
			got.SignedCommandAt, setupNow)
	}
}

// TestCompleteSetupFallsBackWhenTheRereadFails: losing the re-read degrades to
// the pre-probe row rather than refusing to finish the owner's setup. The cost
// of a stale epoch reading is bounded at one extra push; the cost of refusing
// is the whole feature.
func TestCompleteSetupFallsBackWhenTheRereadFails(t *testing.T) {
	row := setupRow(withEpoch(2*time.Hour, 0))
	h := newSetupHarness([]VehicleSnapshotRow{row}, func(h *setupHarness) {
		h.vehicles.err = errors.New("pool exhausted")
	})

	state, err := h.completer.Complete(context.Background(), row)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if state.State != SetupStateConfiguring {
		t.Errorf("state = %q, want %q", state.State, SetupStateConfiguring)
	}
	if got := h.repusher.count(); got != 1 {
		t.Errorf("forced pushes = %d, want 1", got)
	}
}

// TestEpochForceSpentIsOnePredicate pins the bound itself, and that the
// reconciler's own gate delegates to it rather than restating it — a second
// copy would be free to drift.
func TestEpochForceSpentIsOnePredicate(t *testing.T) {
	tests := []struct {
		name string
		c    FleetConfigCandidate
		want bool
	}{
		{name: "never forced", c: FleetConfigCandidate{SignedCommandAt: setupNow}, want: false},
		{
			name: "forced after the epoch opened",
			c:    FleetConfigCandidate{SignedCommandAt: setupNow.Add(-time.Hour), ForcedRepushAt: setupNow},
			want: true,
		},
		{
			name: "forced at exactly the epoch instant still counts as spent",
			c:    FleetConfigCandidate{SignedCommandAt: setupNow, ForcedRepushAt: setupNow},
			want: true,
		},
		{
			name: "a newer epoch re-arms it",
			c:    FleetConfigCandidate{SignedCommandAt: setupNow, ForcedRepushAt: setupNow.Add(-time.Hour)},
			want: false,
		},
		{
			// No epoch ever observed, but we HAVE forced: spent, so a car we
			// can never prove paired costs one escalation and then nothing.
			name: "forced with no epoch is spent",
			c:    FleetConfigCandidate{ForcedRepushAt: setupNow},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := epochForceSpent(tt.c); got != tt.want {
				t.Errorf("epochForceSpent = %v, want %v", got, tt.want)
			}

			// The reconciler's gate must agree, given its other two gates pass.
			c := tt.c
			c.LastOutcome = outcomeSyncedQuiet
			c.LastAttemptAt = setupNow.Add(-24 * time.Hour)
			r := newTestReconciler(&fakeCandidateLister{}, &fakeConfigReader{}, &fakeConfigWriter{}, okToken())
			r.now = func() time.Time { return setupNow }
			if _, ok := r.escalationBlocker(c); ok == tt.want {
				t.Errorf("escalationBlocker allowed = %v; it must mirror epochForceSpent = %v", ok, tt.want)
			}
		})
	}
}
