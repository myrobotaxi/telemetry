package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// Integration tests for the RESEND path's SIBLING SCOPE.
//
// A multi-vehicle invite is ONE code backing N rows. Resend therefore has to
// re-mint every one of those rows or it does not do what it says: re-minting
// only the row whose id is in the path leaves the old code live and pending on
// the siblings, which (a) keeps a leaked code redeemable for the rest of its
// 7-day TTL, defeating the entire reason an owner presses "resend", and (b)
// splits one invite in two, so redeeming the new code grants a single car and
// redeeming the old one grants the rest.
//
// These tests are integration tests because what is under test is the WHERE
// clause of a conditional UPDATE and the transaction that wraps it — neither of
// which a mock would exercise.

// pendingCodeFor reads the live code off a vehicle's single pending row.
//
//nolint:unparam // owner is spelled at every call site on purpose: these are access-control tests and an implicit actor would make them unreadable
func pendingCodeFor(t *testing.T, repo *store.VehicleShareRepo, vehicleID, owner string) string {
	t.Helper()
	rows, err := repo.ListInvitesForVehicle(context.Background(), vehicleID, owner)
	if err != nil {
		t.Fatalf("list %s: %v", vehicleID, err)
	}
	if len(rows) != 1 {
		t.Fatalf("vehicle %s has %d invites, want exactly 1", vehicleID, len(rows))
	}
	return rows[0].Code
}

func TestVehicleShareRepo_ResendIsAtomicAcrossSiblings(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, _ := seedShareFixtures(t)
	repo := newShareRepo(t)

	t.Run("re-mints EVERY row of a multi-vehicle invite, not just the path row", func(t *testing.T) {
		cleanVehicleShares(t)
		original := mustCreateInvite(t, repo, shareOwnerA, vehA1,
			[]string{vehA1, vehA2}, store.SharePermissionRides)

		updated, err := repo.ResendInvite(ctx, original.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("resend: %v", err)
		}
		if updated.Code == original.Code {
			t.Fatal("resend reused the previous code")
		}

		// Both rows must now carry the SAME new code. A sibling still holding
		// the old one is a live credential the owner believes they revoked.
		for _, vehicleID := range []string{vehA1, vehA2} {
			got := pendingCodeFor(t, repo, vehicleID, shareOwnerA)
			if got == original.Code {
				t.Errorf("vehicle %s still carries the PREVIOUS code after a resend — "+
					"the old code stays redeemable for the rest of its 7-day TTL", vehicleID)
			}
			if got != updated.Code {
				t.Errorf("vehicle %s carries a different code than the resent row — "+
					"a multi-vehicle invite is ONE code", vehicleID)
			}
		}

		// The response is still the PATH row, unchanged in identity.
		if updated.ID != original.ID {
			t.Errorf("resend changed the invite id (%s → %s)", original.ID, updated.ID)
		}
		if updated.VehicleID != vehA1 {
			t.Errorf("resend returned the row for %s, want the path vehicle %s", updated.VehicleID, vehA1)
		}
		if !updated.CreatedAt.Equal(original.CreatedAt) {
			t.Error("resend moved createdAt on the path row")
		}
	})

	t.Run("the OLD code redeems NOTHING after a resend", func(t *testing.T) {
		cleanVehicleShares(t)
		original := mustCreateInvite(t, repo, shareOwnerA, vehA1,
			[]string{vehA1, vehA2}, store.SharePermissionRides)
		if _, err := repo.ResendInvite(ctx, original.ID, shareOwnerA); err != nil {
			t.Fatalf("resend: %v", err)
		}

		// This is the whole point of resend-after-leak: whoever holds the old
		// code must get the same answer as somebody guessing at random.
		grants, err := repo.RedeemCode(ctx, original.Code, shareViewer1)
		if !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("redeeming the invalidated code: err = %v, want not-found (granted %d)", err, len(grants))
		}
	})

	t.Run("the NEW code grants EVERY vehicle atomically", func(t *testing.T) {
		cleanVehicleShares(t)
		original := mustCreateInvite(t, repo, shareOwnerA, vehA1,
			[]string{vehA1, vehA2}, store.SharePermissionRides)
		updated, err := repo.ResendInvite(ctx, original.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("resend: %v", err)
		}

		grants, err := repo.RedeemCode(ctx, updated.Code, shareViewer1)
		if err != nil {
			t.Fatalf("redeem the resent code: %v", err)
		}
		if len(grants) != 2 {
			t.Fatalf("the resent code granted %d vehicle(s), want 2 — a resend must not split the invite", len(grants))
		}
		granted := map[string]bool{}
		for _, g := range grants {
			granted[g.VehicleID] = true
		}
		for _, vehicleID := range []string{vehA1, vehA2} {
			if !granted[vehicleID] {
				t.Errorf("the resent code did not grant %s", vehicleID)
			}
		}
	})

	t.Run("a resend does NOT disturb the owner's OTHER invites", func(t *testing.T) {
		cleanVehicleShares(t)
		// Two independent single-vehicle invites from the same owner. They
		// carry DIFFERENT codes, so the sibling re-mint must key on the code,
		// never on the owner alone.
		target := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)
		bystander := mustCreateInvite(t, repo, shareOwnerA, vehA2, []string{vehA2}, store.SharePermissionLive)

		if _, err := repo.ResendInvite(ctx, target.ID, shareOwnerA); err != nil {
			t.Fatalf("resend: %v", err)
		}

		if got := pendingCodeFor(t, repo, vehA2, shareOwnerA); got != bystander.Code {
			t.Error("resending one invite re-minted an UNRELATED invite of the same owner — " +
				"the sibling set is the rows sharing the target's code, not every pending row")
		}
	})

	t.Run("single-vehicle resend still re-mints exactly its one row", func(t *testing.T) {
		cleanVehicleShares(t)
		original := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)

		updated, err := repo.ResendInvite(ctx, original.ID, shareOwnerA)
		if err != nil {
			t.Fatalf("resend: %v", err)
		}
		if updated.Code == original.Code {
			t.Error("resend reused the previous code")
		}
		if !updated.ExpiresAt.After(original.ExpiresAt) {
			t.Errorf("resend did not push the expiry out (%v → %v)", original.ExpiresAt, updated.ExpiresAt)
		}
		if got := pendingCodeFor(t, repo, vehA1, shareOwnerA); got != updated.Code {
			t.Error("the single row does not carry the returned code")
		}
		// The old code is dead here too.
		if _, err := repo.RedeemCode(ctx, original.Code, shareViewer1); !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("the previous single-vehicle code still redeems: %v", err)
		}
	})

	t.Run("another owner cannot resend, and nothing moves", func(t *testing.T) {
		cleanVehicleShares(t)
		original := mustCreateInvite(t, repo, shareOwnerA, vehA1,
			[]string{vehA1, vehA2}, store.SharePermissionLive)

		if _, err := repo.ResendInvite(ctx, original.ID, shareOwnerB); !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("err = %v, want not-found", err)
		}
		for _, vehicleID := range []string{vehA1, vehA2} {
			if got := pendingCodeFor(t, repo, vehicleID, shareOwnerA); got != original.Code {
				t.Errorf("a rejected resend still re-minted %s", vehicleID)
			}
		}
	})
}
