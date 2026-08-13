package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// MYR-540 — the group ride against a real database: the accept-time mint's
// exactly-once guard, the join's error matrix and idempotence, and the batched
// member read.

const (
	groupJoinerA = "cjoiner540a"
	groupJoinerB = "cjoiner540b"
)

// groupRide is minimalRideRequest with the group toggle on.
func groupRide() store.RideRequestRecord {
	rec := minimalRideRequest()
	rec.GroupRide = true
	return rec
}

// acceptGroupRide creates a group ride and runs the owner's accept through the
// SAME guarded write production uses — which is where the mint lives, so a test
// that stamped a code by hand would be testing nothing.
func acceptGroupRide(t *testing.T, repo *store.RideRequestRepo, in store.RideRequestRecord) store.RideRequestRecord {
	t.Helper()
	ctx := context.Background()
	created, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	accepted, err := repo.UpdateStatusFromUnconflicted(ctx, created.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested}, store.RideRequestStatusAccepted)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return accepted
}

// TestRideGroup_AcceptMintsExactlyOnce pins the two halves of the mint's guard:
// a group ride gets a code AT ACCEPT, and nothing mints a second one.
func TestRideGroup_AcceptMintsExactlyOnce(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	accepted := acceptGroupRide(t, repo, groupRide())
	if accepted.JoinCode == "" {
		t.Fatal("an accepted group ride carries no join code; the requester has nothing to share")
	}
	if accepted.JoinCodeExpiresAt == nil {
		t.Fatal("the code has no expiry; the link could never be signed")
	}
	first := accepted.JoinCode

	// A SECOND mint attempt — the shape a re-delivered accept or a future second
	// caller would take. It must change nothing: a re-mint would silently
	// invalidate every link already sent.
	code, _, err := repo.MintJoinCode(ctx, testPool, accepted.ID)
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if code != "" {
		t.Fatalf("a second mint produced %q; the first link would have died", code)
	}

	after, err := repo.GetByID(ctx, accepted.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if after.JoinCode != first {
		t.Errorf("code changed from %q to %q under a repeat mint", first, after.JoinCode)
	}
}

// TestRideGroup_SoloRideNeverMints is the other half of the same guard, and the
// one that makes "shareUrl only on a group ride" a database fact rather than a
// projection rule somebody could get wrong.
func TestRideGroup_SoloRideNeverMints(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	accepted := acceptGroupRide(t, repo, minimalRideRequest()) // group toggle OFF
	if accepted.JoinCode != "" {
		t.Fatalf("a solo ride minted %q; it is not joinable and never will be", accepted.JoinCode)
	}

	code, _, err := repo.MintJoinCode(ctx, testPool, accepted.ID)
	if err != nil {
		t.Fatalf("explicit mint on a solo ride: %v", err)
	}
	if code != "" {
		t.Fatalf("an explicit mint on a solo ride produced %q", code)
	}
}

// TestRideGroup_JoinErrorMatrix walks every refusal the redemption can produce.
func TestRideGroup_JoinErrorMatrix(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	accepted := acceptGroupRide(t, repo, groupRide())

	t.Run("an unknown code is not found", func(t *testing.T) {
		if _, _, err := repo.JoinRideByCode(ctx, "ZZZZZZ", groupJoinerA); !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("err = %v, want sdk.ErrNotFound", err)
		}
	})

	t.Run("the requester cannot join their own ride", func(t *testing.T) {
		_, _, err := repo.JoinRideByCode(ctx, accepted.JoinCode, accepted.RiderID)
		if !errors.Is(err, store.ErrRideJoinSelfParty) {
			t.Fatalf("err = %v, want ErrRideJoinSelfParty", err)
		}
	})

	t.Run("the vehicle owner cannot join either", func(t *testing.T) {
		_, _, err := repo.JoinRideByCode(ctx, accepted.JoinCode, accepted.OwnerID)
		if !errors.Is(err, store.ErrRideJoinSelfParty) {
			t.Fatalf("err = %v, want ErrRideJoinSelfParty", err)
		}
	})

	t.Run("a good code joins and returns the joiner in members", func(t *testing.T) {
		rec, created, err := repo.JoinRideByCode(ctx, accepted.JoinCode, groupJoinerA)
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		if !created {
			t.Error("created = false on a first join")
		}
		if len(rec.Members) != 1 || rec.Members[0].UserID != groupJoinerA {
			t.Fatalf("members = %+v, want the caller present — the joiner goes straight to the tracking sheet", rec.Members)
		}
		if rec.Members[0].FirstName == "" {
			t.Error("firstName is empty; the ladder must always produce a printable name")
		}
	})

	t.Run("re-joining is idempotent", func(t *testing.T) {
		rec, created, err := repo.JoinRideByCode(ctx, accepted.JoinCode, groupJoinerA)
		if err != nil {
			t.Fatalf("re-join: %v", err)
		}
		if created {
			t.Error("created = true on a re-join; the event would fire twice")
		}
		if len(rec.Members) != 1 {
			t.Fatalf("members = %d after a re-join, want 1 — a retry must not add a row", len(rec.Members))
		}
	})
}

// TestRideGroup_TerminalRideRefusesTheCode is the access story's ending: the
// code dies with the ride, at terminal, with no grace at all — and it is
// INDISTINGUISHABLE from an unknown code, which is what keeps the endpoint from
// being an oracle.
func TestRideGroup_TerminalRideRefusesTheCode(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	accepted := acceptGroupRide(t, repo, groupRide())

	if _, err := repo.UpdateStatusFrom(ctx, accepted.ID,
		[]store.RideRequestStatus{store.RideRequestStatusAccepted},
		store.RideRequestStatusCancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	_, _, err := repo.JoinRideByCode(ctx, accepted.JoinCode, groupJoinerA)
	if !errors.Is(err, sdk.ErrNotFound) {
		t.Fatalf("err = %v, want sdk.ErrNotFound — the same answer an unknown code gets", err)
	}
}

// TestRideGroup_MembersAreBatchedOnAPage pins the read pattern MYR-539
// established: a whole page of rides attaches its members with ONE statement,
// never one per row. It is asserted through the observable consequence — every
// ride on the page carries its own members, in join order — because the count
// itself is a property of the SQL, which the metrics stub does not expose.
func TestRideGroup_MembersAreBatchedOnAPage(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	rideA := acceptGroupRide(t, repo, groupRide())
	if _, _, err := repo.JoinRideByCode(ctx, rideA.JoinCode, groupJoinerA); err != nil {
		t.Fatalf("join A: %v", err)
	}
	if _, _, err := repo.JoinRideByCode(ctx, rideA.JoinCode, groupJoinerB); err != nil {
		t.Fatalf("join B: %v", err)
	}

	page, err := repo.ListByRiderPage(ctx, rideA.RiderID, store.RideRequestListCursor{}, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *store.RideRequestRecord
	for i := range page.Items {
		if page.Items[i].ID == rideA.ID {
			found = &page.Items[i]
		}
	}
	if found == nil {
		t.Fatal("the rider's own group ride is missing from their list")
	}
	if len(found.Members) != 2 {
		t.Fatalf("members on the listed ride = %d, want 2", len(found.Members))
	}
	// Join order is the contract's "server-defined order (currently ascending
	// join time)", and it must be stable across reads.
	if found.Members[0].UserID != groupJoinerA || found.Members[1].UserID != groupJoinerB {
		t.Errorf("members = %+v, want join order", found.Members)
	}
}

// TestRideGroup_MemberSeesTheRideInTheirList pins the §7.8 list widening: a
// member's "my rides" contains the ride they joined, not only the ones they
// booked. Without it a joiner's app would show an empty list while they sat in
// the car.
func TestRideGroup_MemberSeesTheRideInTheirList(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	ride := acceptGroupRide(t, repo, groupRide())
	if _, _, err := repo.JoinRideByCode(ctx, ride.JoinCode, groupJoinerA); err != nil {
		t.Fatalf("join: %v", err)
	}

	page, err := repo.ListByRiderPage(ctx, groupJoinerA, store.RideRequestListCursor{}, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != ride.ID {
		t.Fatalf("the member's list = %+v, want the joined ride", page.Items)
	}

	// And a stranger's list stays empty — the widening is membership, not a
	// hole in the scoping.
	other, err := repo.ListByRiderPage(ctx, "cstranger540", store.RideRequestListCursor{}, 10)
	if err != nil {
		t.Fatalf("list stranger: %v", err)
	}
	if len(other.Items) != 0 {
		t.Fatalf("a non-member's list = %+v, want empty", other.Items)
	}
}

// TestRideGroup_IsRideMemberIgnoresStatus pins the deliberate asymmetry between
// the two membership questions. "Is this person a party to this ride" stays true
// after the ride ends — exactly as it does for the requester and the owner, so a
// member can still read yesterday's ride — while "may they see live telemetry
// from this car" does not.
func TestRideGroup_IsRideMemberIgnoresStatus(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	ride := acceptGroupRide(t, repo, groupRide())
	if _, _, err := repo.JoinRideByCode(ctx, ride.JoinCode, groupJoinerA); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := repo.UpdateStatusFrom(ctx, ride.ID,
		[]store.RideRequestStatus{store.RideRequestStatusAccepted},
		store.RideRequestStatusCompleted); err != nil {
		t.Fatalf("complete: %v", err)
	}

	member, err := repo.IsRideMember(ctx, ride.ID, groupJoinerA)
	if err != nil {
		t.Fatalf("IsRideMember: %v", err)
	}
	if !member {
		t.Error("a member stopped being a party when the ride ended; they could not read their own history")
	}

	ids, err := repo.RideMemberIDs(ctx, ride.ID)
	if err != nil {
		t.Fatalf("RideMemberIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != groupJoinerA {
		t.Errorf("member ids = %v on a completed ride, want the joiner — the cancellation push depends on it", ids)
	}
}
