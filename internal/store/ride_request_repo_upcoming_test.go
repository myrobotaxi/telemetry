package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Integration coverage for the MYR-360 upcoming-reservations slice
// (RideRequestRepo.ListUpcomingByOwnerVehiclePage), the read behind
// `GET /api/ride-requests/incoming?upcomingForVehicle={id}`.
//
// Predicate under test:
//
//	owner_id = $1 AND vehicle_id = $2
//	AND status = 'accepted'
//	AND scheduled_for IS NOT NULL AND scheduled_for > NOW()
//	ORDER BY scheduled_for ASC, id ASC
//
// Every exclusion below is a row an owner must NOT be warned about when they
// pause ride sharing: an undecided request (they can still just decline it), a
// reservation already due (the sweeper owns it), an instant ride (no
// reservation at all), another car, another owner, or a terminal row.

const (
	upcomingVehicleA = "clxyz1234567890abcdef" // == minimalRideRequest().VehicleID
	upcomingVehicleB = "clvehicleb234567890ab"
	upcomingOwnerB   = "clownerb234567890abcd"
)

// upcomingSeed describes one row to plant.
type upcomingSeed struct {
	name      string
	vehicleID string
	ownerID   string
	riderID   string
	offset    time.Duration // from now; zero with instant=true means no reservation
	instant   bool
	status    store.RideRequestStatus
}

// seedRideRow creates the row and moves it to its target status. Returns the
// persisted id.
func seedRideRow(t *testing.T, repo *store.RideRequestRepo, s upcomingSeed) string {
	t.Helper()
	ctx := context.Background()

	in := minimalRideRequest()
	if s.vehicleID != "" {
		in.VehicleID = s.vehicleID
	}
	if s.ownerID != "" {
		in.OwnerID = s.ownerID
	}
	if s.riderID != "" {
		in.RiderID = s.riderID
	}
	if !s.instant {
		at := time.Now().UTC().Add(s.offset)
		in.ScheduledFor = &at
	}

	rec, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatalf("seed %q: Create: %v", s.name, err)
	}
	if s.status != "" && s.status != store.RideRequestStatusRequested {
		if _, err := repo.UpdateStatus(ctx, rec.ID, s.status); err != nil {
			t.Fatalf("seed %q: UpdateStatus(%s): %v", s.name, s.status, err)
		}
	}
	return rec.ID
}

// TestRideRequestRepo_ListUpcomingByOwnerVehicle_Predicate plants one row per
// exclusion reason and asserts only the accepted, future, same-vehicle,
// same-owner reservations come back — soonest first.
func TestRideRequestRepo_ListUpcomingByOwnerVehicle_Predicate(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	owner := minimalRideRequest().OwnerID

	sooner := seedRideRow(t, repo, upcomingSeed{
		name: "accepted future (sooner)", offset: time.Hour, status: store.RideRequestStatusAccepted,
	})
	later := seedRideRow(t, repo, upcomingSeed{
		name: "accepted future (later)", offset: 5 * time.Hour, status: store.RideRequestStatusAccepted,
	})

	excluded := []upcomingSeed{
		{name: "still requested", offset: 2 * time.Hour, status: store.RideRequestStatusRequested},
		{name: "accepted but already due", offset: -time.Hour, status: store.RideRequestStatusAccepted},
		{name: "accepted instant ride", instant: true, status: store.RideRequestStatusAccepted},
		{name: "declined", offset: 3 * time.Hour, status: store.RideRequestStatusDeclined},
		{name: "cancelled", offset: 4 * time.Hour, status: store.RideRequestStatusCancelled},
		{name: "completed", offset: 6 * time.Hour, status: store.RideRequestStatusCompleted},
		{name: "another vehicle", vehicleID: upcomingVehicleB, offset: 30 * time.Minute, status: store.RideRequestStatusAccepted},
		{name: "another owner", ownerID: upcomingOwnerB, offset: 20 * time.Minute, status: store.RideRequestStatusAccepted},
	}
	for _, s := range excluded {
		seedRideRow(t, repo, s)
	}

	page, err := repo.ListUpcomingByOwnerVehiclePage(ctx, owner, upcomingVehicleA, store.RideRequestUpcomingCursor{}, 50)
	if err != nil {
		t.Fatalf("ListUpcomingByOwnerVehiclePage: %v", err)
	}
	if page.HasMore {
		t.Errorf("hasMore: got true, want false on a complete page")
	}
	if len(page.Items) != 2 {
		t.Fatalf("items: got %d want 2 — %v", len(page.Items), idsOf(page.Items))
	}
	// SOONEST FIRST — the dialog names "the NEXT reservation", so the ordering
	// is load-bearing, not cosmetic.
	if page.Items[0].ID != sooner || page.Items[1].ID != later {
		t.Errorf("ordering: got %v want [%s %s] (soonest first)", idsOf(page.Items), sooner, later)
	}
	for _, rec := range page.Items {
		if rec.Status != store.RideRequestStatusAccepted {
			t.Errorf("row %s leaked status %q", rec.ID, rec.Status)
		}
		if rec.ScheduledFor == nil || !rec.ScheduledFor.After(time.Now()) {
			t.Errorf("row %s leaked a non-future reservation: %v", rec.ID, rec.ScheduledFor)
		}
		if rec.VehicleID != upcomingVehicleA || rec.OwnerID != owner {
			t.Errorf("row %s leaked scope: vehicle=%s owner=%s", rec.ID, rec.VehicleID, rec.OwnerID)
		}
	}
}

// TestRideRequestRepo_ListUpcomingByOwnerVehicle_Paginates walks the slice one
// row at a time and asserts the ASCENDING (scheduled_for, id) keyset resumes
// with no gaps and no duplicates — including across a scheduled_for TIE, where
// the id tie-break is the only thing separating two rows.
func TestRideRequestRepo_ListUpcomingByOwnerVehicle_Paginates(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	owner := minimalRideRequest().OwnerID

	// Two distinct instants plus a pair sharing one instant.
	tie := 3 * time.Hour
	for _, off := range []time.Duration{time.Hour, 2 * time.Hour, tie, tie} {
		seedRideRow(t, repo, upcomingSeed{name: "reservation", offset: off, status: store.RideRequestStatusAccepted})
	}
	const total = 4

	seen := make([]string, 0, total)
	var cursor store.RideRequestUpcomingCursor
	for pages := 0; ; pages++ {
		if pages > total+2 {
			t.Fatal("pagination did not terminate")
		}
		page, err := repo.ListUpcomingByOwnerVehiclePage(ctx, owner, upcomingVehicleA, cursor, 1)
		if err != nil {
			t.Fatalf("ListUpcomingByOwnerVehiclePage: %v", err)
		}
		if len(page.Items) > 1 {
			t.Fatalf("page exceeded limit: %d", len(page.Items))
		}
		for _, rec := range page.Items {
			for _, prior := range seen {
				if prior == rec.ID {
					t.Fatalf("duplicate row %s across pages", rec.ID)
				}
			}
			seen = append(seen, rec.ID)
			cursor = store.RideRequestUpcomingCursor{ScheduledFor: *rec.ScheduledFor, ID: rec.ID}
		}
		if !page.HasMore {
			break
		}
	}

	if len(seen) != total {
		t.Fatalf("expected %d distinct rows across pages, got %d (%v)", total, len(seen), seen)
	}

	// The paged walk must reproduce the single-page order exactly.
	full, err := repo.ListUpcomingByOwnerVehiclePage(ctx, owner, upcomingVehicleA, store.RideRequestUpcomingCursor{}, 50)
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

// TestRideRequestRepo_ListUpcomingByOwnerVehicle_UnknownVehicle proves an
// unknown or unowned vehicle is an EMPTY page, not an error — the handler
// turns it into a 200 with `items: []` so the param cannot oracle whether
// somebody else's car exists.
func TestRideRequestRepo_ListUpcomingByOwnerVehiclePage_UnknownVehicle(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	owner := minimalRideRequest().OwnerID

	seedRideRow(t, repo, upcomingSeed{name: "a real reservation", offset: time.Hour, status: store.RideRequestStatusAccepted})

	for _, tt := range []struct {
		name      string
		ownerID   string
		vehicleID string
	}{
		{name: "unknown vehicle", ownerID: owner, vehicleID: "clnosuchvehicle000000"},
		{name: "vehicle owned by somebody else", ownerID: upcomingOwnerB, vehicleID: upcomingVehicleA},
	} {
		t.Run(tt.name, func(t *testing.T) {
			page, err := repo.ListUpcomingByOwnerVehiclePage(ctx, tt.ownerID, tt.vehicleID, store.RideRequestUpcomingCursor{}, 10)
			if err != nil {
				t.Fatalf("ListUpcomingByOwnerVehiclePage: %v", err)
			}
			if page.Items == nil || len(page.Items) != 0 || page.HasMore {
				t.Fatalf("expected an empty non-nil page, got %#v", page)
			}
		})
	}
}

// idsOf projects a page's rows onto their ids for readable failures.
func idsOf(recs []store.RideRequestRecord) []string {
	out := make([]string, 0, len(recs))
	for i := range recs {
		out = append(out, recs[i].ID)
	}
	return out
}
