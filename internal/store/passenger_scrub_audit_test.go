package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store/passengerscrub"
)

// MYR-447 review follow-up: the ONE-TIME legacy passenger scrub must be
// provable after the fact.
//
// The sweeper's 30-day scrub was audited from the start; the one-time binary
// that clears the whole column at deploy was not. That is the run that
// actually removes the bulk of this data, and an unrecorded mass clearing of
// a P1 column is exactly what CG-DL-3 exists to prevent — "did we really
// scrub production, and when?" has to be answerable from the ledger rather
// than from somebody's terminal scrollback.

// failingAuditor rejects every write, so the fail-closed ordering can be
// asserted rather than assumed.
type failingAuditor struct{ calls int }

var errAuditRefused = errors.New("audit refused")

func (f *failingAuditor) RecordPassengerScrub(_ context.Context, _, _ string, _ int) error {
	f.calls++
	return errAuditRefused
}

// TestPassengerScrubWritesGroupedAuditRows pins both the existence of the
// trail and its shape.
func TestPassengerScrubWritesGroupedAuditRows(t *testing.T) {
	setupPassengerScrub(t)
	ctx := context.Background()

	const owner = "user-scrub-audit"
	// Two vehicles, three rides — the grouping must collapse to one row per
	// (owner, vehicle), not one per ride: AuditLog is append-only and can
	// never be compacted, so a fleet-wide legacy scrub emitting a row per
	// ride would be a permanent cost.
	seedRide(t, "audit-a1", owner, "veh-audit-a", "completed", time.Hour, true)
	seedRide(t, "audit-a2", owner, "veh-audit-a", "cancelled", time.Hour, true)
	seedRide(t, "audit-b1", owner, "veh-audit-b", "completed", time.Hour, true)

	if _, err := newScrubber(false).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rows, err := testPool.Query(ctx, `
		SELECT "targetId", "action", "targetType", "metadata"
		FROM "AuditLog" WHERE "userId" = $1 ORDER BY "targetId"`, owner)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer rows.Close()

	seen := map[string]int{}
	for rows.Next() {
		var targetID, action, targetType string
		var meta []byte
		if scanErr := rows.Scan(&targetID, &action, &targetType, &meta); scanErr != nil {
			t.Fatalf("scan audit row: %v", scanErr)
		}
		if action != "ride_passengers_scrubbed" || targetType != "ride_request" {
			t.Errorf("audit row = action %q targetType %q", action, targetType)
		}
		// CG-DL-5: counts and enums only. A passenger name reaching this row
		// would defeat the entire point of the scrub that wrote it.
		for _, needle := range []string{"Passenger", "passengerName", "555"} {
			if strings.Contains(string(meta), needle) {
				t.Errorf("audit metadata leaked %q: %s", needle, meta)
			}
		}
		var m map[string]any
		if jsonErr := json.Unmarshal(meta, &m); jsonErr != nil {
			t.Fatalf("metadata is not JSON: %v", jsonErr)
		}
		count, ok := m["rideCount"].(float64)
		if !ok {
			t.Fatalf("metadata has no numeric rideCount: %s", meta)
		}
		seen[targetID] = int(count)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("got %d audit rows, want 2 — one per (owner, vehicle): %v", len(seen), seen)
	}
	if seen["veh-audit-a"] != 2 || seen["veh-audit-b"] != 1 {
		t.Errorf("grouped counts = %v, want veh-audit-a:2 veh-audit-b:1", seen)
	}
}

// TestPassengerScrubAbortsWhenTheAuditFails is the fail-closed half. A failure
// to RECORD the scrub must prevent the scrub rather than proceeding silently —
// the same ordering CG-DL-3 requires of a deletion.
func TestPassengerScrubAbortsWhenTheAuditFails(t *testing.T) {
	setupPassengerScrub(t)
	ctx := context.Background()

	seedRide(t, "audit-refused", "user-audit-refused", "veh-audit-refused", "completed", time.Hour, true)

	auditor := &failingAuditor{}
	_, err := passengerscrub.New(testPool, scrubLogger(), false).WithAuditor(auditor).Run(ctx)
	if err == nil {
		t.Fatal("expected the run to abort when the audit write fails")
	}
	if auditor.calls == 0 {
		t.Error("the auditor was never called — the ordering is not audit-first")
	}
	// The column must be untouched: nothing was recorded, so nothing may have
	// happened.
	if name, _ := ridePassenger(t, "audit-refused"); name == nil {
		t.Error("the row was scrubbed despite the audit failing — the run is not fail-closed")
	}
}

// TestPassengerScrubRefusesWithoutAnAuditor guards the seam itself, so a
// future caller that forgets to wire one gets an error rather than a silent
// untracked scrub.
func TestPassengerScrubRefusesWithoutAnAuditor(t *testing.T) {
	setupPassengerScrub(t)
	if _, err := passengerscrub.New(testPool, scrubLogger(), false).Run(context.Background()); err == nil {
		t.Fatal("a real run with no Auditor must be refused")
	}
	// A dry run changes nothing, so it needs no audit row and must still work.
	if _, err := passengerscrub.New(testPool, scrubLogger(), true).Run(context.Background()); err != nil {
		t.Errorf("a dry run must not require an Auditor: %v", err)
	}
}
