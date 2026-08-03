package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Integration coverage for the MYR-400 active-rides slice
// (RideRequestRepo.ListActiveByOwnerVehiclePage), the read behind
// `GET /api/ride-requests/incoming?activeForVehicle={id}`.
//
// Predicate under test:
//
//	owner_id = $1 AND vehicle_id = $2
//	AND (status IN ('arrived','enroute')
//	     OR (status = 'accepted' AND <MYR-376 not-dormant>))
//	ORDER BY created_at DESC, id DESC
//
// The exclusions each say something different. An undecided request is not a
// commitment. A terminal row has freed the car. A FUTURE reservation is the
// sibling `upcomingForVehicle` slice's row, not this one — that boundary is the
// whole reason the two params are complementary rather than overlapping.

const (
	activeVehicleA = "clxyz1234567890abcdef" // == minimalRideRequest().VehicleID
	activeVehicleB = "clvehicleb400567890ab"
	activeOwnerB   = "clownerb400567890abcd"
	activeRiderB   = "clriderb400567890abcd"
	activeRiderC   = "clriderc400567890abcd"
)

// plantDispatchStatus force-sets a seeded row's leg-1 dispatch outcome. Used to
// build the one row the CLOCK alone cannot describe: a reservation the sweeper
// already dispatched. See plantScheduledFor for why read-path fixtures are
// planted rather than driven through the write path.
func plantDispatchStatus(t *testing.T, id, status string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_requests SET dispatch_status = $2 WHERE id = $1`, id, status); err != nil {
		t.Fatalf("plant dispatch_status on %s: %v", id, err)
	}
}

// TestRideRequestRepo_ListActiveByOwnerVehicle_Predicate plants one row per
// inclusion and exclusion reason and asserts only the live ones come back.
func TestRideRequestRepo_ListActiveByOwnerVehicle_Predicate(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	owner := minimalRideRequest().OwnerID

	// An INSTANT accepted ride: the car is rolling to a pickup right now.
	instant := seedRideRow(t, repo, ownerVehicleSeed{
		name: "accepted instant ride", instant: true, status: store.RideRequestStatusAccepted,
	})
	// A reservation PAST its due instant: dormancy has lifted, so the owner may
	// proceed manually (§7.8) and the ride is live. It is exempt from the
	// per-vehicle one-active-ride index (partial on `scheduled_for IS NULL`),
	// which is exactly why this read returns a LIST and not a single row.
	due := seedRideRow(t, repo, ownerVehicleSeed{
		name: "reservation past its due instant", offset: -time.Hour, status: store.RideRequestStatusAccepted,
	})

	excluded := []ownerVehicleSeed{
		// Not a commitment — the owner has not decided. This is the default
		// incoming feed's row.
		{name: "still requested (instant)", instant: true, riderID: activeRiderB, status: store.RideRequestStatusRequested},
		{name: "still requested (reservation)", offset: 2 * time.Hour, status: store.RideRequestStatusRequested},
		// THE BOUNDARY WITH `upcomingForVehicle`: a future reservation is
		// DORMANT — the car has been told nothing and the ride cannot even be
		// picked up (MYR-376). It belongs to the upcoming slice, not this one.
		{name: "accepted but still future (dormant)", offset: 5 * time.Hour, status: store.RideRequestStatusAccepted},
		// Terminal — the car is free again.
		{name: "declined", offset: 3 * time.Hour, status: store.RideRequestStatusDeclined},
		{name: "cancelled", offset: 4 * time.Hour, status: store.RideRequestStatusCancelled},
		{name: "completed", offset: 6 * time.Hour, status: store.RideRequestStatusCompleted},
		// Out of scope.
		{name: "another vehicle", vehicleID: activeVehicleB, riderID: activeRiderC, instant: true, status: store.RideRequestStatusEnroute},
		{name: "another owner", ownerID: activeOwnerB, offset: -30 * time.Minute, status: store.RideRequestStatusAccepted},
	}
	for _, s := range excluded {
		seedRideRow(t, repo, s)
	}

	page, err := repo.ListActiveByOwnerVehiclePage(ctx, owner, activeVehicleA, store.RideRequestListCursor{}, 50)
	if err != nil {
		t.Fatalf("ListActiveByOwnerVehiclePage: %v", err)
	}
	if page.HasMore {
		t.Errorf("hasMore: got true, want false on a complete page")
	}
	got := idsOf(page.Items)
	if len(got) != 2 {
		t.Fatalf("items: got %d want 2 — %v", len(got), got)
	}
	for _, want := range []string{instant, due} {
		if !containsID(got, want) {
			t.Errorf("missing live ride %s from %v", want, got)
		}
	}
	// NEWEST FIRST — the feed's default ordering, which this slice keeps.
	if page.Items[0].CreatedAt.Before(page.Items[1].CreatedAt) {
		t.Errorf("ordering: not created_at DESC — %v then %v",
			page.Items[0].CreatedAt, page.Items[1].CreatedAt)
	}
	for _, rec := range page.Items {
		if rec.VehicleID != activeVehicleA || rec.OwnerID != owner {
			t.Errorf("row %s leaked scope: vehicle=%s owner=%s", rec.ID, rec.VehicleID, rec.OwnerID)
		}
	}
}

// TestRideRequestRepo_ListActiveByOwnerVehicle_CommittedStatuses proves all
// THREE committed statuses come back. An owner recovering the ride on a second
// device may be at any point in the two-leg handshake, so a slice that only
// returned `accepted` would strand exactly the rides furthest along.
//
// Each status gets a fresh database because the per-vehicle one-active-ride
// index permits only one committed INSTANT ride per car at a time — the very
// invariant this status set is derived from.
func TestRideRequestRepo_ListActiveByOwnerVehicle_CommittedStatuses(t *testing.T) {
	for _, status := range []store.RideRequestStatus{
		store.RideRequestStatusAccepted,
		store.RideRequestStatusArrived,
		store.RideRequestStatusEnroute,
	} {
		t.Run(string(status), func(t *testing.T) {
			repo, _ := setupRideRequestRepo(t)
			ctx := context.Background()
			owner := minimalRideRequest().OwnerID

			id := seedRideRow(t, repo, ownerVehicleSeed{name: "live ride", instant: true, status: status})

			page, err := repo.ListActiveByOwnerVehiclePage(ctx, owner, activeVehicleA, store.RideRequestListCursor{}, 50)
			if err != nil {
				t.Fatalf("ListActiveByOwnerVehiclePage: %v", err)
			}
			if len(page.Items) != 1 || page.Items[0].ID != id {
				t.Fatalf("expected the %s ride %s, got %v", status, id, idsOf(page.Items))
			}
			if page.Items[0].Status != status {
				t.Errorf("status: got %q want %q", page.Items[0].Status, status)
			}
		})
	}
}

// TestRideRequestRepo_ListActiveByOwnerVehicle_DispatchedReservation pins that
// the slice reuses MYR-376's dormancy predicate WHOLE, including its dispatch
// arm — not a clock-only approximation of it.
//
// A reservation whose leg-1 nav actually reached the car is LIVE whatever the
// clock says (§7.8: "a dispatched reservation is live whatever the clock
// says"), and it is precisely the ride an owner most needs to recover. A
// hand-rolled `scheduled_for <= NOW()` would silently drop it.
func TestRideRequestRepo_ListActiveByOwnerVehicle_DispatchedReservation(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	owner := minimalRideRequest().OwnerID

	dispatched := seedRideRow(t, repo, ownerVehicleSeed{
		name:   "future reservation the sweeper already dispatched",
		offset: time.Hour, status: store.RideRequestStatusAccepted,
	})
	plantDispatchStatus(t, dispatched, "sent")

	// Its undispatched twin, same shape, must stay out.
	seedRideRow(t, repo, ownerVehicleSeed{
		name: "future reservation, undispatched", offset: 2 * time.Hour, status: store.RideRequestStatusAccepted,
	})

	page, err := repo.ListActiveByOwnerVehiclePage(ctx, owner, activeVehicleA, store.RideRequestListCursor{}, 50)
	if err != nil {
		t.Fatalf("ListActiveByOwnerVehiclePage: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != dispatched {
		t.Fatalf("expected only the dispatched reservation %s, got %v", dispatched, idsOf(page.Items))
	}
}

// TestRideRequestRepo_ListActiveByOwnerVehicle_Paginates walks the slice one
// row at a time and asserts the DESCENDING (created_at, id) keyset resumes with
// no gaps and no duplicates — the same discipline as its siblings.
func TestRideRequestRepo_ListActiveByOwnerVehicle_Paginates(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	owner := minimalRideRequest().OwnerID

	// One instant ride plus two DUE reservations: reservations are exempt from
	// the per-vehicle one-active-ride index, so more than one row on a car is
	// reachable and the pagination is not decoration.
	seedRideRow(t, repo, ownerVehicleSeed{name: "instant", instant: true, status: store.RideRequestStatusAccepted})
	for _, off := range []time.Duration{-time.Hour, -2 * time.Hour} {
		seedRideRow(t, repo, ownerVehicleSeed{name: "due reservation", offset: off, status: store.RideRequestStatusAccepted})
	}
	const total = 3

	seen := make([]string, 0, total)
	var cursor store.RideRequestListCursor
	for pages := 0; ; pages++ {
		if pages > total+2 {
			t.Fatal("pagination did not terminate")
		}
		page, err := repo.ListActiveByOwnerVehiclePage(ctx, owner, activeVehicleA, cursor, 1)
		if err != nil {
			t.Fatalf("ListActiveByOwnerVehiclePage: %v", err)
		}
		if len(page.Items) > 1 {
			t.Fatalf("page exceeded limit: %d", len(page.Items))
		}
		for _, rec := range page.Items {
			if containsID(seen, rec.ID) {
				t.Fatalf("duplicate row %s across pages", rec.ID)
			}
			seen = append(seen, rec.ID)
			cursor = store.RideRequestListCursor{CreatedAt: rec.CreatedAt, ID: rec.ID}
		}
		if !page.HasMore {
			break
		}
	}

	if len(seen) != total {
		t.Fatalf("expected %d distinct rows across pages, got %d (%v)", total, len(seen), seen)
	}

	// The paged walk must reproduce the single-page order exactly.
	full, err := repo.ListActiveByOwnerVehiclePage(ctx, owner, activeVehicleA, store.RideRequestListCursor{}, 50)
	if err != nil {
		t.Fatalf("full page: %v", err)
	}
	want := idsOf(full.Items)
	if len(want) != total {
		t.Fatalf("full page returned %d rows, want %d", len(want), total)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("paged order %v != single-page order %v", seen, want)
		}
	}
}

// TestRideRequestRepo_ListActiveByOwnerVehiclePage_UnknownVehicle proves an
// unknown or unowned vehicle is an EMPTY page, not an error — the handler turns
// it into a 200 with `items: []` so the param cannot oracle whether somebody
// else's car exists. A caller who is not the owner therefore learns nothing:
// the only thing that distinguishes "not yours" from "yours, idle" is nothing.
func TestRideRequestRepo_ListActiveByOwnerVehiclePage_UnknownVehicle(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	owner := minimalRideRequest().OwnerID

	seedRideRow(t, repo, ownerVehicleSeed{name: "a real live ride", instant: true, status: store.RideRequestStatusEnroute})

	for _, tt := range []struct {
		name      string
		ownerID   string
		vehicleID string
	}{
		{name: "unknown vehicle", ownerID: owner, vehicleID: "clnosuchvehicle000000"},
		{name: "vehicle owned by somebody else", ownerID: activeOwnerB, vehicleID: activeVehicleA},
	} {
		t.Run(tt.name, func(t *testing.T) {
			page, err := repo.ListActiveByOwnerVehiclePage(ctx, tt.ownerID, tt.vehicleID, store.RideRequestListCursor{}, 10)
			if err != nil {
				t.Fatalf("ListActiveByOwnerVehiclePage: %v", err)
			}
			if page.Items == nil || len(page.Items) != 0 || page.HasMore {
				t.Fatalf("expected an empty non-nil page, got %#v", page)
			}
		})
	}
}

// TestRideRequestRepo_ListActiveByOwnerVehicle_IdleCar is the ordinary state:
// the owner's car is serving nothing, and that is a 200 with no rows rather
// than an error.
func TestRideRequestRepo_ListActiveByOwnerVehicle_IdleCar(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	owner := minimalRideRequest().OwnerID

	page, err := repo.ListActiveByOwnerVehiclePage(ctx, owner, activeVehicleA, store.RideRequestListCursor{}, 10)
	if err != nil {
		t.Fatalf("ListActiveByOwnerVehiclePage: %v", err)
	}
	if page.Items == nil || len(page.Items) != 0 || page.HasMore {
		t.Fatalf("expected an empty non-nil page, got %#v", page)
	}
}

// containsID reports whether ids holds want.
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
