package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-581 — `ProfileNameRepo.UpdateUserName` against a real Postgres.
//
// THE DEFECT THIS TEST EXISTS FOR. The first version of this writer was a single
// `UPDATE "User" SET "name"`. An APPLE-NATIVE account is minted in `go_users` +
// `go_identity_apple` and has no Prisma `"User"` row at all, so that statement
// matched zero rows and `PATCH /api/users/me` answered 404 — to every nameless
// Apple user, which is the entire population the endpoint exists for.
//
// THE ASSERTION THAT MATTERS is not "the write landed somewhere". It is that
// EVERY LADDER READER RESOLVES THE NEW NAME afterwards, including when a STALE
// name sits on a HIGHER rung than the one an account "belongs" to. That is the
// trap a "write the account's own table" fix falls into silently: the Apple
// first-consent name outranks `go_users`, so writing only `go_users` would report
// success while every reader went on serving the old name.
//
// The reader used here is `VehicleNameRepo.RequesterFirstName`, deliberately: it
// runs `queryOwnerFirstNameSources`, the SAME three-rung ladder behind
// `RideRequest.requesterName`, `VehicleSummary.ownerFirstName`,
// `ShareInvite.acceptedByName` and the redeem response. Proving it through a real
// reader rather than by inspecting rows is what makes this a test of the
// requirement instead of a test of the implementation.
func TestProfileNameRepo_UpdateUserName(t *testing.T) {
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := store.NewProfileNameRepo(testPool)
	// A REAL ladder reader — the point of the test is what readers resolve, not
	// which rows moved.
	reader := store.NewVehicleNameRepo(testPool)

	t.Run("a legacy web account (Prisma User row only) is renamed", func(t *testing.T) {
		const id = "usr_pn_prisma"
		seedUser(t, id, strPtr("Old Name"), strPtr("prisma@example.com"))
		t.Cleanup(func() { cleanProfileNameRows(t, id) })

		updated, err := repo.UpdateUserName(ctx, id, "Alexandra")
		if err != nil {
			t.Fatalf("UpdateUserName: %v", err)
		}
		if !updated {
			t.Fatal("a Prisma-only account must be renameable")
		}
		assertLadderFirstName(t, reader, id, "Alexandra")
	})

	// THE REGRESSION ARM. No `"User"` row exists for this id at all.
	t.Run("an APPLE-NATIVE account (no Prisma User row) is renamed", func(t *testing.T) {
		const id = "usr_pn_apple"
		seedGoUser(t, id, nil, strPtr("apple@example.com"))
		seedAppleIdentity(t, "apple_sub_pn_1", id, nil, strPtr("apple@example.com"))
		t.Cleanup(func() { cleanProfileNameRows(t, id) })

		updated, err := repo.UpdateUserName(ctx, id, "James")
		if err != nil {
			t.Fatalf("UpdateUserName: %v", err)
		}
		if !updated {
			t.Fatal("an Apple-native account must be renameable — this is the 404 the fix removes")
		}
		assertLadderFirstName(t, reader, id, "James")
	})

	// THE PRECEDENCE ARM, and the reason the writer touches every rung. The
	// Apple binding carries a STALE name that outranks `go_users`; a fix that
	// wrote only the account's "own" table would leave "Jimmy" winning forever.
	t.Run("a stale name on a HIGHER rung does not survive the rename", func(t *testing.T) {
		const id = "usr_pn_stale"
		seedGoUser(t, id, strPtr("Jim"), strPtr("stale@example.com"))
		seedAppleIdentity(t, "apple_sub_pn_2", id, strPtr("Jimmy Apple"), strPtr("stale@example.com"))
		t.Cleanup(func() { cleanProfileNameRows(t, id) })

		// Precondition: the higher rung is what the ladder currently serves.
		assertLadderFirstName(t, reader, id, "Jimmy")

		if _, err := repo.UpdateUserName(ctx, id, "James"); err != nil {
			t.Fatalf("UpdateUserName: %v", err)
		}
		assertLadderFirstName(t, reader, id, "James")
	})

	// The full three-rung account: every rung must move, so no reader anywhere
	// can serve a stale name regardless of which rung it consults.
	t.Run("every rung the account has is updated together", func(t *testing.T) {
		const id = "usr_pn_all"
		seedUser(t, id, strPtr("Old Prisma"), strPtr("all@example.com"))
		seedGoUser(t, id, strPtr("Old Go"), strPtr("all2@example.com"))
		seedAppleIdentity(t, "apple_sub_pn_3", id, strPtr("Old Apple"), strPtr("all@example.com"))
		t.Cleanup(func() { cleanProfileNameRows(t, id) })

		if _, err := repo.UpdateUserName(ctx, id, "Mira Chen"); err != nil {
			t.Fatalf("UpdateUserName: %v", err)
		}
		for _, q := range []struct {
			label string
			sql   string
		}{
			{`"User"`, `SELECT "name" FROM "User" WHERE "id" = $1`},
			{"go_identity_apple", `SELECT name FROM go_identity_apple WHERE user_id = $1`},
			{"go_users", `SELECT name FROM go_users WHERE id = $1`},
		} {
			var got *string
			if err := testPool.QueryRow(ctx, q.sql, id).Scan(&got); err != nil {
				t.Fatalf("read %s name: %v", q.label, err)
			}
			if got == nil || *got != "Mira Chen" {
				t.Errorf("%s carries %v, want the new full name — a rung left behind can win the COALESCE",
					q.label, got)
			}
		}
		// The FULL name is stored; the first-token reduction is a READ-time
		// concern, so the ladder reader is what shortens it.
		assertLadderFirstName(t, reader, id, "Mira")
	})

	// EVERY binding row, not one. `go_identity_apple.user_id` is not unique, and
	// the ladder's middle rung picks the most recent login — so a rename that
	// updated one row would work or not depending on which device signed in last.
	t.Run("all Apple binding rows for one user are updated", func(t *testing.T) {
		const id = "usr_pn_multi"
		seedGoUser(t, id, nil, strPtr("multi@example.com"))
		seedAppleIdentity(t, "apple_sub_pn_4a", id, strPtr("Old A"), strPtr("multi@example.com"))
		seedAppleIdentity(t, "apple_sub_pn_4b", id, strPtr("Old B"), nil)
		t.Cleanup(func() { cleanProfileNameRows(t, id) })

		if _, err := repo.UpdateUserName(ctx, id, "Nabil"); err != nil {
			t.Fatalf("UpdateUserName: %v", err)
		}
		var stale int
		if err := testPool.QueryRow(ctx,
			`SELECT COUNT(*) FROM go_identity_apple WHERE user_id = $1 AND name <> $2`,
			id, "Nabil").Scan(&stale); err != nil {
			t.Fatalf("count stale bindings: %v", err)
		}
		if stale != 0 {
			t.Errorf("%d Apple binding rows still carry the old name — which one wins depends on login order", stale)
		}
	})

	// THE NEW MEANING OF THE 404. Not "no Prisma User row" (which was routine)
	// but "no row on ANY rung" — an orphaned token.
	t.Run("an id with no row on any rung reports not-updated", func(t *testing.T) {
		updated, err := repo.UpdateUserName(ctx, "usr_pn_ghost", "Nobody")
		if err != nil {
			t.Fatalf("UpdateUserName: %v", err)
		}
		if updated {
			t.Error("an id with no identity row anywhere must report not-updated — that is the 404 arm")
		}
	})

	t.Run("the empty string is refused loudly, never reported as not-found", func(t *testing.T) {
		updated, err := repo.UpdateUserName(ctx, "usr_pn_ghost", "")
		if err == nil {
			t.Error("an empty name must be an ERROR — it is a deletion this endpoint does not offer")
		}
		if updated {
			t.Error("an empty name must not report success")
		}
	})
}

// assertLadderFirstName runs a REAL ladder reader and checks the first name it
// resolves.
func assertLadderFirstName(t *testing.T, reader *store.VehicleNameRepo, userID, want string) {
	t.Helper()
	got, err := reader.RequesterFirstName(context.Background(), userID)
	if err != nil {
		t.Fatalf("RequesterFirstName(%s): %v", userID, err)
	}
	if got != want {
		t.Fatalf("the ladder resolves %q for %s, want %q", got, userID, want)
	}
}

// cleanProfileNameRows removes one test account from all three rungs. Per-subtest
// rather than a blanket sweep so the arms cannot depend on each other's leftovers.
func cleanProfileNameRows(t *testing.T, userID string) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM go_identity_apple WHERE user_id = $1`,
		`DELETE FROM go_users WHERE id = $1`,
		`DELETE FROM "User" WHERE "id" = $1`,
	} {
		if _, err := testPool.Exec(ctx, q, userID); err != nil {
			t.Fatalf("clean profile-name rows (%s): %v", userID, err)
		}
	}
}
