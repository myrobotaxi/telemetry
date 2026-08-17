package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-583 — a name only counts once its owner has CONFIRMED it, against a real
// Postgres.
//
// The client's ruling, verbatim: "Any names not entered/confirmed should be in
// Setting Up. So please ensure even cars with names but not confirmed show that
// label." Every assertion below is that sentence applied to one surface.
//
// WHY THESE TESTS NEED A DATABASE AND COULD NOT BE UNIT TESTS. The whole feature
// is a conjunction inside SQL that three statements share. A fake could only
// re-state the conjunction in Go, which is the one place it is NOT implemented.
// What is exercised here is the real ladder, the real EXISTS probe, and the real
// transaction boundary.

// TestProfileNameConfirmation_WriteAndGate covers the two halves of the feature
// on ONE account and ONE car, deliberately: the wire value and the offerability
// gate are derived from a single SQL expression precisely so they cannot disagree,
// so the test that matters reads BOTH on every arm and checks they agree.
func TestProfileNameConfirmation_WriteAndGate(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)
	ctx := context.Background()

	const (
		ownerID   = "cnamec00000000000000owner"
		vehicleID = "cnamec0000000000000000veh"
	)
	name := "Ada Lovelace"
	email := ownerID + "@example.com"
	seedUser(t, ownerID, &name, &email)
	seedVehicleSummaryRow(t, vehicleID, ownerID, vinForIndex(811), "Car", "Model 3", 2024,
		"white", store.VehicleStatusParked, 70, 220)
	t.Cleanup(func() {
		cleanNameConfirmations(t, ownerID)
		cleanProfileNameRows(t, ownerID)
	})

	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})

	// readBoth reads the catalog's emitted first name and the gate's boolean off
	// the two statements a rider and the sweeper actually use.
	readBoth := func(t *testing.T) (*string, bool) {
		t.Helper()
		summaries, err := repo.ListSummariesByUser(ctx, ownerID)
		if err != nil {
			t.Fatalf("ListSummariesByUser: %v", err)
		}
		if len(summaries) != 1 {
			t.Fatalf("catalog returned %d rows, want 1", len(summaries))
		}
		state, err := repo.GetDispatchState(ctx, vehicleID)
		if err != nil {
			t.Fatalf("GetDispatchState: %v", err)
		}
		return summaries[0].OwnerFirstName, state.OwnerNamed
	}

	tests := []struct {
		name string
		// confirmed decides whether a confirmation row exists for this arm.
		confirmed bool
		// wantFirstName is the catalog's emitted value; empty means null.
		wantFirstName string
	}{
		{
			// THE ARM THE ISSUE IS ABOUT. A resolvable legacy name — this one is
			// on the TOP rung, the Prisma "User" row, exactly where the web-era
			// placeholder and the Apple first-consent name land — that nobody
			// ever confirmed. It must read as ABSENT to a counterparty and the
			// car must not be offerable.
			name:          "a named but UNCONFIRMED owner emits null and is not offerable",
			confirmed:     false,
			wantFirstName: "",
		},
		{
			name:          "a named and CONFIRMED owner emits the first name and is offerable",
			confirmed:     true,
			wantFirstName: "Ada",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cleanNameConfirmations(t, ownerID)
			if tc.confirmed {
				seedNameConfirmation(t, ownerID)
			}

			gotName, gotNamed := readBoth(t)

			switch {
			case tc.wantFirstName == "" && gotName != nil:
				t.Errorf("catalog emitted %q, want null", *gotName)
			case tc.wantFirstName != "" && gotName == nil:
				t.Errorf("catalog emitted null, want %q", tc.wantFirstName)
			case tc.wantFirstName != "" && *gotName != tc.wantFirstName:
				t.Errorf("catalog emitted %q, want %q — the first-token reduction still applies",
					*gotName, tc.wantFirstName)
			}

			// THE INVARIANT THE WHOLE DESIGN RESTS ON: the emit is non-null
			// exactly when the gate says named. A row that shows a name while
			// the gate refuses the car (or the reverse) is the worst available
			// outcome on this pair, because one is then lying about the very
			// fact the other enforces.
			if wantNamed := tc.wantFirstName != ""; gotNamed != wantNamed {
				t.Errorf("gate says named=%v while the wire emitted %v — "+
					"the shared ladder has drifted", gotNamed, gotName)
			}
		})
	}
}

// TestProfileNameRepo_ConfirmsOnWrite is the WRITE half: a successful
// PATCH /api/users/me is what mints the confirmation, in the same transaction as
// the name.
func TestProfileNameRepo_ConfirmsOnWrite(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := store.NewProfileNameRepo(testPool)

	t.Run("a successful rename confirms the caller", func(t *testing.T) {
		const id = "usr_pnc_ok"
		seedGoUser(t, id, nil, strPtr("pnc-ok@example.com"))
		t.Cleanup(func() { cleanNameConfirmations(t, id); cleanProfileNameRows(t, id) })

		if confirmed := nameConfirmedAt(t, id); confirmed != nil {
			t.Fatal("precondition: a fresh account must not be confirmed")
		}
		updated, err := repo.UpdateUserName(ctx, id, "James")
		if err != nil {
			t.Fatalf("UpdateUserName: %v", err)
		}
		if !updated {
			t.Fatal("the rename must report success")
		}
		if nameConfirmedAt(t, id) == nil {
			t.Error("a successful rename must record a confirmation — it IS the confirmation")
		}
	})

	t.Run("a second rename re-confirms rather than failing on the key", func(t *testing.T) {
		const id = "usr_pnc_twice"
		seedGoUser(t, id, nil, strPtr("pnc-twice@example.com"))
		t.Cleanup(func() { cleanNameConfirmations(t, id); cleanProfileNameRows(t, id) })

		if _, err := repo.UpdateUserName(ctx, id, "James"); err != nil {
			t.Fatalf("first UpdateUserName: %v", err)
		}
		first := nameConfirmedAt(t, id)
		if first == nil {
			t.Fatal("first rename did not confirm")
		}
		// The UPSERT: a person editing their name twice must not meet a
		// primary-key violation on the confirmation row.
		if _, err := repo.UpdateUserName(ctx, id, "Jim"); err != nil {
			t.Fatalf("second UpdateUserName: %v", err)
		}
		second := nameConfirmedAt(t, id)
		if second == nil {
			t.Fatal("second rename lost the confirmation")
		}
		if second.Before(*first) {
			t.Error("confirmed_at went backwards — the upsert must record the LATEST confirmation")
		}
	})

	// A FAILED NAME WRITE CONFIRMS NOTHING, and this is the arm that matters
	// most: a confirmation committed without a name would mark a STALE name —
	// an Apple-consent name, or the legacy web placeholder — as approved by the
	// person, permanently and invisibly. That is the exact state the feature
	// exists to eliminate.
	t.Run("an orphaned token (no rung matched) confirms nothing", func(t *testing.T) {
		const id = "usr_pnc_ghost"
		t.Cleanup(func() { cleanNameConfirmations(t, id) })

		updated, err := repo.UpdateUserName(ctx, id, "Nobody")
		if err != nil {
			t.Fatalf("UpdateUserName: %v", err)
		}
		if updated {
			t.Fatal("an id with no identity row anywhere must report not-updated")
		}
		if nameConfirmedAt(t, id) != nil {
			t.Error("nothing was named, so nothing may be confirmed — " +
				"a confirmation without a name asserts approval of a name that does not exist")
		}
	})

	t.Run("a refused empty name confirms nothing", func(t *testing.T) {
		const id = "usr_pnc_empty"
		seedGoUser(t, id, strPtr("Existing"), strPtr("pnc-empty@example.com"))
		t.Cleanup(func() { cleanNameConfirmations(t, id); cleanProfileNameRows(t, id) })

		if _, err := repo.UpdateUserName(ctx, id, ""); err == nil {
			t.Fatal("an empty name must be refused")
		}
		if nameConfirmedAt(t, id) != nil {
			t.Error("a refused write must not confirm the name it did not write")
		}
	})
}

// TestVehicleShareRepo_AcceptedByNameRequiresConfirmation is the §7.5.2 half: the
// GRANTEE's name on the owner's Share tab is gated on the grantee's own
// confirmation, mirroring the owner-side gate exactly.
func TestVehicleShareRepo_AcceptedByNameRequiresConfirmation(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, _, _ := seedShareFixtures(t)
	repo := newShareRepo(t)
	t.Cleanup(func() { cleanNameConfirmations(t, shareViewer1) })

	invite := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)
	if _, err := repo.RedeemCode(ctx, invite.Code, shareViewer1); err != nil {
		t.Fatalf("RedeemCode: %v", err)
	}

	// seedShareFixtures gives every fixture account the two-token name
	// "Alex Owner" on the Prisma rung and confirms nothing — the legacy state.
	acceptedName := func(t *testing.T) *string {
		t.Helper()
		rows, err := repo.ListInvitesForVehicle(ctx, vehA1, shareOwnerA)
		if err != nil {
			t.Fatalf("ListInvitesForVehicle: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("listed %d rows, want 1", len(rows))
		}
		if rows[0].Status != store.ShareStatusAccepted {
			t.Fatalf("status = %q, want accepted", rows[0].Status)
		}
		return rows[0].AcceptedByName
	}

	t.Run("an accepted row whose grantee never confirmed OMITS the name", func(t *testing.T) {
		cleanNameConfirmations(t, shareViewer1)
		if got := acceptedName(t); got != nil {
			t.Errorf("acceptedByName = %q, want omitted — the grantee never confirmed that name", *got)
		}
	})

	t.Run("a confirmed grantee is named, first token only", func(t *testing.T) {
		seedNameConfirmation(t, shareViewer1)
		got := acceptedName(t)
		if got == nil {
			t.Fatal("acceptedByName is omitted for a CONFIRMED grantee")
		}
		if *got != "Alex" {
			t.Errorf("acceptedByName = %q, want %q — first names only to a counterparty", *got, "Alex")
		}
	})

	// The confirmation probe keys on `accepted_by_user_id`, which is NULL on a
	// pending row, so a pending row resolves absent with no status guard. Worth
	// asserting because that is free correctness rather than written correctness,
	// and free correctness is what a refactor silently removes.
	t.Run("a PENDING row is still nameless, confirmed grantees or not", func(t *testing.T) {
		cleanVehicleShares(t)
		pending := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)
		if pending.AcceptedByName != nil {
			t.Errorf("a pending invite carries acceptedByName = %q", *pending.AcceptedByName)
		}
	})
}

// seedNameConfirmation records a confirmation for one account, as the PATCH
// endpoint's transaction does. Idempotent, matching the production upsert.
func seedNameConfirmation(t *testing.T, userID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_profile_name_confirmations (user_id, confirmed_at)
		 VALUES ($1, NOW())
		 ON CONFLICT (user_id) DO UPDATE SET confirmed_at = NOW()`, userID); err != nil {
		t.Fatalf("seed name confirmation (%s): %v", userID, err)
	}
}

// cleanNameConfirmations removes one account's confirmation, so an arm cannot
// inherit the previous arm's consent.
func cleanNameConfirmations(t *testing.T, userID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`DELETE FROM go_profile_name_confirmations WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("clean name confirmations (%s): %v", userID, err)
	}
}

// nameConfirmedAt returns the confirmation timestamp, or nil when the account has
// never confirmed. Row presence is the signal the readers use; this returns the
// timestamp so the upsert's re-confirmation can be observed.
func nameConfirmedAt(t *testing.T, userID string) *time.Time {
	t.Helper()
	var at *time.Time
	err := testPool.QueryRow(context.Background(),
		`SELECT confirmed_at FROM go_profile_name_confirmations WHERE user_id = $1`, userID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatalf("read name confirmation (%s): %v", userID, err)
	}
	return at
}
