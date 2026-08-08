package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-451 regression: a grant whose ride capability the owner WITHDREW must
// read as "no rides" from every enforcement surface, against the real database.
//
// WHY THIS EXISTS WHEN TestEndpointGrantMatrix ALREADY COVERS CREATE. That
// matrix drives the real handlers, but it resolves grants through a FAKE share
// reader — it proves the handler refuses a grant that arrives carrying
// AllowRides=false, and proves nothing whatever about whether the database ever
// produces one. The entire persistence half (the UPDATE, the flag's survival of
// a round trip, the SELECT the gate actually issues) sat below the fake. MYR-451
// was reported as a persistence/serialization defect precisely there, and no
// test could have distinguished a broken write from a working one.
//
// So this drives the production statements: patch the capability off, then ask
// every read the ride path depends on and require all of them to agree.
func TestMYR451_WithdrawnRideCapabilityIsRefusedEverywhere(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, _, _ := seedShareFixtures(t)
	repo := newShareRepo(t)

	// Redeem at the RIDES preset, so the grant genuinely starts with the
	// capability. Starting from `live` would let a test pass that never
	// exercised a withdrawal at all.
	id := acceptedGrantFixture(t, repo, vehA1, store.SharePermissionRides)

	allowRides, err := repo.ShareGrantFor(ctx, shareViewer1, vehA1)
	if err != nil {
		t.Fatalf("ShareGrantFor before patch: %v", err)
	}
	if !allowRides {
		t.Fatal("fixture did not redeem with the ride capability; the withdrawal below would prove nothing")
	}
	beforePatch := grantUpdatedAt(t, id)

	// The owner's toggle.
	row, err := repo.PatchInvite(ctx, store.PatchShareInviteInput{
		InviteID: id, OwnerUserID: shareOwnerA, AllowRides: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("PatchInvite(allowRides=false): %v", err)
	}
	if row.AllowRides {
		t.Fatal("the write did not land: the echoed row still carries the ride capability")
	}

	// THE GATE'S OWN READ. vehicleAccessFor -> ShareGrantFor is the single
	// statement standing between a grantee and POST /api/ride-requests; if it
	// answers true here the create returns 201 and MYR-451 is real.
	allowRides, err = repo.ShareGrantFor(ctx, shareViewer1, vehA1)
	if err != nil {
		t.Fatalf("ShareGrantFor after patch: %v", err)
	}
	if allowRides {
		t.Error("CREATE GATE WOULD ADMIT: the grant still reads allow_rides=true after the owner withdrew it")
	}

	// THE SWEEPER'S READ. A reservation accepted before the withdrawal is
	// claimed by the dispatcher, which resolves through a different statement
	// and must reach the same verdict — otherwise the capability is enforced at
	// the door and ignored at the moment the car is actually committed.
	permitted, err := repo.RiderMayRequestRides(ctx, shareViewer1, vehA1)
	if err != nil {
		t.Fatalf("RiderMayRequestRides: %v", err)
	}
	if permitted {
		t.Error("DISPATCH WOULD CLAIM: the sweeper still believes the rider may ride")
	}

	// THE PRODUCTION ROW SHAPE. The recovered row read `permission = 'rides'`
	// beside `allow_rides = false`, and that pairing is what made the report
	// look like corruption. It is not: `permission` is the invite-time preset
	// and is deliberately never patchable, so a narrowed grant is SUPPOSED to
	// look exactly like this. Pinning it here stops a future "fix" from
	// rewriting the preset to match the flag — which would destroy the only
	// evidence that a withdrawal ever happened.
	if got := grantPermission(t, id); got != store.SharePermissionRides {
		t.Errorf("permission = %q, want %q — the invite-time preset must survive a capability withdrawal",
			got, store.SharePermissionRides)
	}

	// THE FORENSIC STAMP (0032). Without this the incident could not be dated,
	// which is why the withdrawal could not be placed before or after the rides
	// the grantee took.
	afterPatch := grantUpdatedAt(t, id)
	if !afterPatch.After(beforePatch) {
		t.Errorf("updated_at did not advance across the patch (before=%s after=%s); a capability change is undateable again",
			beforePatch, afterPatch)
	}
}

// TestMYR451_RedemptionStampsTheCapabilityItGrants covers the SECOND statement
// that moves a capability.
//
// Redemption derives `allow_rides` from the invite's preset, so for a `rides`
// invite it flips the flag false → true. If it did not stamp `updated_at`, a
// grant that acquired the ride capability at redemption would still report the
// INVITE's creation instant — and an on-call asking MYR-451's own question
// ("did this grant have rides before or after the ride they took?") would read
// a date that can be days early. That is the same class of wrong answer the
// column was added to prevent, so the fix is only half done without this.
func TestMYR451_RedemptionStampsTheCapabilityItGrants(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ctx := context.Background()
	vehA1, _, _ := seedShareFixtures(t)
	repo := newShareRepo(t)

	invite := mustCreateInvite(t, repo, shareOwnerA, vehA1, []string{vehA1}, store.SharePermissionRides)

	var createdAt time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT created_at FROM go_vehicle_shares WHERE id = $1`, invite.ID).Scan(&createdAt); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	if _, err := repo.RedeemCode(ctx, invite.Code, shareViewer1); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	// created_at and updated_at are written by separate statements, so their
	// NOW() values come from different transactions and must differ.
	updatedAt := grantUpdatedAt(t, invite.ID)
	if !updatedAt.After(createdAt) {
		t.Errorf("redemption granted the ride capability without stamping updated_at (created=%s updated=%s); the capability's acquisition is misdated to the invite instant",
			createdAt, updatedAt)
	}
}

// grantUpdatedAt reads the MYR-451 mutation stamp for one grant.
func grantUpdatedAt(t *testing.T, inviteID string) time.Time {
	t.Helper()
	var at time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT updated_at FROM go_vehicle_shares WHERE id = $1`, inviteID).Scan(&at); err != nil {
		t.Fatalf("read updated_at(%s): %v", inviteID, err)
	}
	return at
}

// grantPermission reads the invite-time preset for one grant.
func grantPermission(t *testing.T, inviteID string) string {
	t.Helper()
	var permission string
	if err := testPool.QueryRow(context.Background(),
		`SELECT permission FROM go_vehicle_shares WHERE id = $1`, inviteID).Scan(&permission); err != nil {
		t.Fatalf("read permission(%s): %v", inviteID, err)
	}
	return permission
}
