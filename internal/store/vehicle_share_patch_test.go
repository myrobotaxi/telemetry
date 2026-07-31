package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// MYR-369 PatchInvite: the owner editing one accepted grant in place, and the
// suspension invariant reaching every access-set read.

// acceptedGrantFixture mints an invite at the given preset, redeems it as
// shareViewer1, and returns the accepted row's id.
func acceptedGrantFixture(t *testing.T, repo *store.VehicleShareRepo, vehicleID, permission string) string {
	t.Helper()
	ctx := context.Background()
	invite := mustCreateInvite(t, repo, shareOwnerA, vehicleID, []string{vehicleID}, permission)
	if _, err := repo.RedeemCode(ctx, invite.Code, shareViewer1); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return invite.ID
}

func boolPtr(b bool) *bool { return &b }

func TestVehicleShareRepo_PatchInvite(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, _, _ := seedShareFixtures(t)
	repo := newShareRepo(t)

	t.Run("flips allow_rides and echoes the stored row", func(t *testing.T) {
		cleanVehicleShares(t)
		id := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)

		row, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: id, OwnerUserID: shareOwnerA, AllowRides: boolPtr(true),
		})
		if err != nil {
			t.Fatalf("PatchInvite: %v", err)
		}
		if !row.AllowRides {
			t.Error("allow_rides did not flip")
		}
		if row.SuspendedAt != nil {
			t.Error("an allowRides-only patch suspended the grant")
		}
		// The gate reads the same row a moment later and must agree.
		allowRides, err := repo.ShareGrantFor(ctx, shareViewer1, vehA1)
		if err != nil {
			t.Fatalf("ShareGrantFor after patch: %v", err)
		}
		if !allowRides {
			t.Error("the gate still reads the pre-patch capability")
		}
	})

	t.Run("PARTIAL: an absent field is untouched, an explicit false is a change", func(t *testing.T) {
		cleanVehicleShares(t)
		id := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionRides)

		// Suspend WITHOUT mentioning allowRides. The capability must survive
		// — this is the partial-update bug the pointer fields exist to
		// prevent, and here it would silently strip a capability the owner
		// never touched.
		row, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: id, OwnerUserID: shareOwnerA, Suspended: boolPtr(true),
		})
		if err != nil {
			t.Fatalf("PatchInvite(suspend): %v", err)
		}
		if !row.AllowRides {
			t.Fatal("suspending cleared allow_rides — an absent field must be left alone")
		}
		if row.SuspendedAt == nil {
			t.Fatal("suspended_at was not stamped")
		}

		// Now take the capability away explicitly, leaving the suspension.
		row, err = repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: id, OwnerUserID: shareOwnerA, AllowRides: boolPtr(false),
		})
		if err != nil {
			t.Fatalf("PatchInvite(revoke rides): %v", err)
		}
		if row.AllowRides {
			t.Error("an explicit false did not clear allow_rides")
		}
		if row.SuspendedAt == nil {
			t.Error("clearing allow_rides also un-suspended the grant")
		}
	})

	t.Run("SUSPENSION GATES EVERYTHING: the grant vanishes from every access read", func(t *testing.T) {
		cleanVehicleShares(t)
		id := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionRides)
		vehicles := store.NewVehicleRepo(testPool, store.NoopMetrics{})

		// Before: visible everywhere.
		if ids, err := repo.SharedVehicleIDs(ctx, shareViewer1); err != nil || len(ids) != 1 {
			t.Fatalf("pre-suspend access set = %v (err %v), want one vehicle", ids, err)
		}

		if _, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: id, OwnerUserID: shareOwnerA, Suspended: boolPtr(true),
		}); err != nil {
			t.Fatalf("suspend: %v", err)
		}

		// After: gone from the access set...
		ids, err := repo.SharedVehicleIDs(ctx, shareViewer1)
		if err != nil {
			t.Fatalf("SharedVehicleIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Errorf("a suspended grant is still in the access set: %v", ids)
		}
		// ...from the per-vehicle grant lookup, indistinguishably from no
		// grant at all...
		if _, err := repo.ShareGrantFor(ctx, shareViewer1, vehA1); !errors.Is(err, sdk.ErrNotFound) {
			t.Errorf("ShareGrantFor on a suspended grant: err = %v, want not-found", err)
		}
		// ...from the viewer catalog, as an ABSENT ROW rather than a
		// decorated one...
		rows, err := vehicles.ListSharedSummariesByUser(ctx, shareViewer1)
		if err != nil {
			t.Fatalf("ListSharedSummariesByUser: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("a suspended grant still produces a catalog row: %v", rows)
		}
		// ...and from the reservation-dispatch probe.
		permitted, err := repo.RiderMayRequestRides(ctx, shareViewer1, vehA1)
		if err != nil {
			t.Fatalf("RiderMayRequestRides: %v", err)
		}
		if permitted {
			t.Error("a suspended grant still permits a reservation dispatch")
		}
	})

	t.Run("restoring returns exactly the capabilities the row already held", func(t *testing.T) {
		cleanVehicleShares(t)
		id := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionRides)

		for _, suspended := range []bool{true, false} {
			if _, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
				InviteID: id, OwnerUserID: shareOwnerA, Suspended: boolPtr(suspended),
			}); err != nil {
				t.Fatalf("PatchInvite(suspended=%v): %v", suspended, err)
			}
		}

		allowRides, err := repo.ShareGrantFor(ctx, shareViewer1, vehA1)
		if err != nil {
			t.Fatalf("ShareGrantFor after restore: %v", err)
		}
		if !allowRides {
			t.Error("restoring lost the ride capability — nothing should be re-derived")
		}
	})

	t.Run("owner scoping is in the statement: another owner gets not-found", func(t *testing.T) {
		cleanVehicleShares(t)
		id := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)

		_, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: id, OwnerUserID: shareOwnerB, AllowRides: boolPtr(true),
		})
		if !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("cross-owner patch: err = %v, want not-found (indistinguishable from missing)", err)
		}
		// And it must not have written anything.
		allowRides, err := repo.ShareGrantFor(ctx, shareViewer1, vehA1)
		if err != nil {
			t.Fatalf("ShareGrantFor: %v", err)
		}
		if allowRides {
			t.Fatal("a cross-owner patch WROTE — the ownership predicate is not on the UPDATE")
		}
	})

	t.Run("a PENDING invite is not patchable", func(t *testing.T) {
		cleanVehicleShares(t)
		invite := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionLive)

		_, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: invite.ID, OwnerUserID: shareOwnerA, Suspended: boolPtr(true),
		})
		if !errors.Is(err, store.ErrShareNotAccepted) {
			t.Fatalf("patching a pending invite: err = %v, want ErrShareNotAccepted", err)
		}
		// Told plainly rather than hidden — the caller owns the row.
		if errors.Is(err, sdk.ErrNotFound) {
			t.Error("ErrShareNotAccepted must not wrap ErrNotFound; the owner already knows the row exists")
		}
	})

	t.Run("a REVOKED tombstone is not-found, never 'revoked'", func(t *testing.T) {
		cleanVehicleShares(t)
		id := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)
		if _, err := repo.RevokeInvite(ctx, id, shareOwnerA); err != nil {
			t.Fatalf("revoke: %v", err)
		}

		_, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: id, OwnerUserID: shareOwnerA, Suspended: boolPtr(false),
		})
		if !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("patching a tombstone: err = %v, want not-found — the wire never shows revoked rows", err)
		}
	})

	t.Run("an unknown invite id is not-found", func(t *testing.T) {
		cleanVehicleShares(t)
		_, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: "csh-does-not-exist", OwnerUserID: shareOwnerA, Suspended: boolPtr(true),
		})
		if !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("err = %v, want not-found", err)
		}
	})

	t.Run("an empty patch is refused rather than silently succeeding", func(t *testing.T) {
		cleanVehicleShares(t)
		id := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionLive)

		_, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
			InviteID: id, OwnerUserID: shareOwnerA,
		})
		if err == nil {
			t.Fatal("an empty patch succeeded; a client bug must not look like an applied edit")
		}
	})
}

// TestVehicleShareRepo_TierAtRedeem pins the preset → flag mapping, including
// the retired live_history tier, which redeems to the SAME flags as live.
func TestVehicleShareRepo_TierAtRedeem(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, _, _ := seedShareFixtures(t)
	repo := newShareRepo(t)

	tests := []struct {
		preset         string
		wantAllowRides bool
		wantPersisted  string
	}{
		{store.SharePermissionRides, true, store.SharePermissionRides},
		{store.SharePermissionLive, false, store.SharePermissionLive},
		// ACCEPTED AT CREATE so an un-updated client keeps working, but
		// PERSISTED as live: the tier is retired and must never be written
		// again.
		{store.SharePermissionLiveHistory, false, store.SharePermissionLive},
	}
	for _, tt := range tests {
		t.Run(tt.preset, func(t *testing.T) {
			cleanVehicleShares(t)
			invite := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, tt.preset)
			if invite.Permission != tt.wantPersisted {
				t.Errorf("persisted permission = %q, want %q", invite.Permission, tt.wantPersisted)
			}

			grants, err := repo.RedeemCode(ctx, invite.Code, shareViewer1)
			if err != nil {
				t.Fatalf("redeem: %v", err)
			}
			if len(grants) != 1 {
				t.Fatalf("granted %d vehicles, want 1", len(grants))
			}
			if grants[0].AllowRides != tt.wantAllowRides {
				t.Errorf("allowRides = %v, want %v", grants[0].AllowRides, tt.wantAllowRides)
			}
			// Nothing is born suspended.
			if _, err := repo.ShareGrantFor(ctx, shareViewer1, vehA1); err != nil {
				t.Errorf("a freshly redeemed grant does not resolve: %v", err)
			}
		})
	}
}
