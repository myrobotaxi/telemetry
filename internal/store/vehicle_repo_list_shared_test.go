package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestVehicleRepo_ListSharedSummaries covers the viewer half of the catalog.
//
// The property under test is not "does it return rows" — it is that the
// accepted-grant join is the FILTER. Every negative case below is a way the
// query could accidentally return a car the caller was not granted.
func TestVehicleRepo_ListSharedSummaries(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, vehA2, vehB := seedShareFixtures(t)
	shares := newShareRepo(t)
	vehicles := store.NewVehicleRepo(testPool, store.NoopMetrics{})

	// Owner A shares only vehA1 with viewer 1, at the rides preset — so the
	// projected capability is a TRUE that a missing column could not fake.
	invite := mustCreateInvite(t, shares, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionRides)
	if _, err := shares.RedeemCode(ctx, invite.Code, shareViewer1); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	t.Run("returns only granted vehicles, carrying their capability", func(t *testing.T) {
		rows, err := vehicles.ListSharedSummariesByUser(ctx, shareViewer1)
		if err != nil {
			t.Fatalf("ListSharedSummariesByUser: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1 (only vehA1 was shared)", len(rows))
		}
		if rows[0].ID != vehA1 {
			t.Errorf("row is %s, want %s", rows[0].ID, vehA1)
		}
		if !rows[0].AllowRides {
			t.Error("allowRides = false, want true — the rides preset must project onto the flag")
		}
		if rows[0].Name == "" || rows[0].VIN == "" {
			t.Error("the shared projection dropped catalog columns the owner projection carries")
		}
	})

	t.Run("an ungranted user sees nothing", func(t *testing.T) {
		rows, err := vehicles.ListSharedSummariesByUser(ctx, shareViewer2)
		if err != nil {
			t.Fatalf("ListSharedSummariesByUser: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("an ungranted user saw %d shared vehicles", len(rows))
		}
	})

	t.Run("the OWNER's own car is not in their shared list", func(t *testing.T) {
		// Owner A owns vehA1 and vehA2 but holds no grants, so the shared
		// half of their catalog is empty and the owner half (a separate
		// query) is what surfaces their cars. A row appearing here would
		// mean an owner sees their own car twice — once as owner, once as
		// a viewer of themselves.
		rows, err := vehicles.ListSharedSummariesByUser(ctx, shareOwnerA)
		if err != nil {
			t.Fatalf("ListSharedSummariesByUser: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("the owner appeared in their own shared list (%d rows)", len(rows))
		}
	})

	t.Run("the id filter narrows but never widens", func(t *testing.T) {
		// Asking for a car the caller was NOT granted must return nothing,
		// even though the id is real and belongs to somebody. This is the
		// property the redeem response depends on.
		rows, err := vehicles.ListSharedSummariesByIDs(ctx, shareViewer1, []string{vehA1, vehA2, vehB})
		if err != nil {
			t.Fatalf("ListSharedSummariesByIDs: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != vehA1 {
			t.Fatalf("got %d rows (%v), want only the granted vehA1", len(rows), rows)
		}

		none, err := vehicles.ListSharedSummariesByIDs(ctx, shareViewer1, []string{vehB})
		if err != nil {
			t.Fatalf("ListSharedSummariesByIDs: %v", err)
		}
		if len(none) != 0 {
			t.Errorf("an ungranted id returned %d rows", len(none))
		}

		empty, err := vehicles.ListSharedSummariesByIDs(ctx, shareViewer1, nil)
		if err != nil {
			t.Fatalf("ListSharedSummariesByIDs(nil): %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("an empty id set returned %d rows; it must not fall back to 'everything'", len(empty))
		}
	})

	t.Run("revoking removes the row from the shared catalog", func(t *testing.T) {
		if _, err := shares.RevokeInvite(ctx, invite.ID, shareOwnerA); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		rows, err := vehicles.ListSharedSummariesByUser(ctx, shareViewer1)
		if err != nil {
			t.Fatalf("ListSharedSummariesByUser: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("a revoked viewer still sees %d shared vehicles", len(rows))
		}
	})
}
