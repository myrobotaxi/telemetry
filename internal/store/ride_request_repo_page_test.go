package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestRideRequestRepo_ListByRiderPage_Paginates walks the whole result set
// one small page at a time and asserts the keyset cursor resumes with no
// gaps and no duplicates, and that hasMore flips false on the final page.
func TestRideRequestRepo_ListByRiderPage_Paginates(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	const total = 5
	created := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		// Scheduled rides: a single rider may hold many, whereas only one
		// open INSTANT ride is permitted (MYR-230 guard). Pagination is
		// exercised on the rider's full list regardless of scheduled/instant.
		rec, err := repo.Create(ctx, scheduledRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		created[rec.ID] = true
	}

	seen := make(map[string]bool, total)
	var cursor store.RideRequestListCursor
	pages := 0
	for {
		page, err := repo.ListByRiderPage(ctx, minimalRideRequest().RiderID, cursor, 2)
		if err != nil {
			t.Fatalf("ListByRiderPage: %v", err)
		}
		pages++
		for _, rec := range page.Items {
			if seen[rec.ID] {
				t.Fatalf("duplicate row %s across pages", rec.ID)
			}
			seen[rec.ID] = true
			last := rec
			cursor = store.RideRequestListCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		}
		if len(page.Items) > 2 {
			t.Fatalf("page exceeded limit: %d", len(page.Items))
		}
		if !page.HasMore {
			break
		}
		if pages > total+2 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != total {
		t.Fatalf("expected %d distinct rows across pages, got %d", total, len(seen))
	}
	for id := range created {
		if !seen[id] {
			t.Errorf("row %s never returned", id)
		}
	}
}

// TestRideRequestRepo_ListByOwnerPage_StatusAndCursor proves the owner feed
// respects the status filter across the cursor path and returns an empty,
// non-nil page (hasMore false) for an owner with no rows.
func TestRideRequestRepo_ListByOwnerPage_StatusAndCursor(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	var requestedIDs []string
	for i := 0; i < 3; i++ {
		// Scheduled: several open rides for one rider only coexist when they
		// are not instant (MYR-230 guard). The owner feed filters by status,
		// not by scheduled/instant, so the assertions are unaffected.
		rec, err := repo.Create(ctx, scheduledRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		requestedIDs = append(requestedIDs, rec.ID)
	}
	// Move one to accepted so the status filter must exclude it.
	if _, err := repo.UpdateStatus(ctx, requestedIDs[0], store.RideRequestStatusAccepted); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	owner := minimalRideRequest().OwnerID
	requested := store.RideRequestStatusRequested

	// First page (limit 1) then resume — expect exactly the 2 requested rows.
	page, err := repo.ListByOwnerPage(ctx, owner, &requested, store.RideRequestListCursor{}, 1)
	if err != nil {
		t.Fatalf("ListByOwnerPage: %v", err)
	}
	if len(page.Items) != 1 || !page.HasMore {
		t.Fatalf("first page: items=%d hasMore=%v", len(page.Items), page.HasMore)
	}
	if page.Items[0].Status != store.RideRequestStatusRequested {
		t.Errorf("status filter leaked: %q", page.Items[0].Status)
	}
	last := page.Items[0]

	page2, err := repo.ListByOwnerPage(ctx, owner, &requested, store.RideRequestListCursor{CreatedAt: last.CreatedAt, ID: last.ID}, 10)
	if err != nil {
		t.Fatalf("ListByOwnerPage page2: %v", err)
	}
	if len(page2.Items) != 1 || page2.HasMore {
		t.Fatalf("second page: items=%d hasMore=%v", len(page2.Items), page2.HasMore)
	}
	if page2.Items[0].ID == requestedIDs[0] {
		t.Error("accepted row must not appear in a requested-filtered feed")
	}

	// Owner with no rows: empty non-nil page.
	empty, err := repo.ListByOwnerPage(ctx, "clnobody00000000000000", nil, store.RideRequestListCursor{}, 10)
	if err != nil {
		t.Fatalf("empty ListByOwnerPage: %v", err)
	}
	if empty.Items == nil || len(empty.Items) != 0 || empty.HasMore {
		t.Fatalf("expected empty non-nil page, got %#v", empty)
	}
}
