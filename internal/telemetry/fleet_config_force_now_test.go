package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestForceConfigRepushNowIsTheSameAction proves the on-demand entry point is
// an ENTRY POINT and not a second implementation: same delete-then-create, in
// that order, through the same writer (the signing proxy), with the same epoch
// bookkeeping a background escalation writes.
//
// The ordering assertion is the substantive one. A "reuse" that POSTed before
// deleting would pass a naive both-were-called check while doing the exact
// thing MYR-489 argued against — re-issuing a config Tesla already reports as
// synced, which is the no-op that left Nabil's car silent for two days.
func TestForceConfigRepushNowIsTheSameAction(t *testing.T) {
	h := newForceHarness(nil, nil)
	c := syncedQuietCandidate(nil)[0]

	if !h.rec.ForceConfigRepushNow(context.Background(), c, "tok") {
		t.Fatal("ForceConfigRepushNow reported not-applied on a healthy push")
	}

	if h.writer.deleteCalls != 1 || h.writer.calls != 1 {
		t.Fatalf("delete=%d push=%d, want 1 and 1", h.writer.deleteCalls, h.writer.calls)
	}
	if len(h.writer.order) != 2 || h.writer.order[0] != "delete" || h.writer.order[1] != "push" {
		t.Errorf("write order = %v, want [delete push]", h.writer.order)
	}
	// The epoch budget must be stamped through the FORCED recorder, not the
	// ordinary one — that write is what stops a second force this epoch.
	if len(h.attempts.forced) != 1 || h.attempts.forced[0].outcome != outcomeForcedRepush {
		t.Errorf("forced writes = %+v, want one %q", h.attempts.forced, outcomeForcedRepush)
	}
	if len(h.attempts.cleared) != 0 {
		t.Errorf("cleared = %v, want none — a forced push must NOT drop the schedule", h.attempts.cleared)
	}
}

// TestForceConfigRepushNowReportsFailureHonestly: the caller has an owner
// waiting on an answer, so a create that did not apply must come back false —
// and must still stamp the epoch, or a retry loop could force forever.
func TestForceConfigRepushNowReportsFailureHonestly(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*forceHarness)
		// wantOutcome is the label the schedule write should carry.
		wantOutcome string
	}{
		{
			name: "create errors after a successful delete",
			setup: func(h *forceHarness) {
				h.writer.err = errors.New("upstream 500")
			},
			wantOutcome: outcomeForcedRepushFail,
		},
		{
			name: "Tesla skips the vehicle on the create",
			setup: func(h *forceHarness) {
				h.writer.result = &FleetConfigResponse{Response: FleetConfigResult{
					SkippedVehicles: map[string]string{reconTestVIN: "missing_key"},
				}}
			},
			wantOutcome: outcomeForcedRepushFail,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newForceHarness(nil, tt.setup)
			c := syncedQuietCandidate(nil)[0]

			if h.rec.ForceConfigRepushNow(context.Background(), c, "tok") {
				t.Fatal("reported applied on a failed create")
			}
			if len(h.attempts.forced) != 1 || h.attempts.forced[0].outcome != tt.wantOutcome {
				t.Errorf("forced writes = %+v, want one %q (the epoch must still be spent)",
					h.attempts.forced, tt.wantOutcome)
			}
		})
	}
}

// TestForceConfigRepushNowToleratesAMissingConfig: the delete 404s for a car
// that was never configured, which is the ordinary case for a brand-new owner
// completing setup. Delete-then-create must degrade to create rather than
// refuse.
func TestForceConfigRepushNowToleratesAMissingConfig(t *testing.T) {
	h := newForceHarness(nil, func(h *forceHarness) {
		h.writer.deleteErr = &FleetAPIError{StatusCode: 404, Body: `{"error":"not found"}`}
	})
	c := syncedQuietCandidate(func(c *FleetConfigCandidate) {
		c.LastOutcome = outcomeAwaitingKey
		c.ForcedRepushAt = time.Time{}
	})[0]

	if !h.rec.ForceConfigRepushNow(context.Background(), c, "tok") {
		t.Fatal("a 404 on the delete must not fail the escalation")
	}
	if h.writer.calls != 1 {
		t.Errorf("push calls = %d, want 1", h.writer.calls)
	}
}
