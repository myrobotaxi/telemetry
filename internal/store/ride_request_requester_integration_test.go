package store_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// seedUser inserts a Prisma-owned "User" row (nullable name/email) so the
// requester-name resolution (MYR-229) has an identity to read. name/email are
// passed as *string so tests can exercise the NULL-column branches.
func seedUser(t *testing.T, id string, name, email *string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "User" ("id", "name", "email") VALUES ($1, $2, $3)`,
		id, name, email); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// TestRideRequestRepo_RequesterName_SingleRow exercises the GetByID join/lookup
// across the whole fallback chain plus the no-user-row omission case.
func TestRideRequestRepo_RequesterName_SingleRow(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		userName *string
		email    *string
		seed     bool // whether a "User" row exists at all
		want     string
	}{
		{name: "first name from display name", userName: strPtr("Maya Chen"), email: strPtr("maya@example.com"), seed: true, want: "Maya"},
		{name: "email local-part when name absent", userName: nil, email: strPtr("jordan.lee@example.com"), seed: true, want: "jordan.lee"},
		{name: "Rider literal when name and email absent", userName: nil, email: nil, seed: true, want: "Rider"},
		{name: "omitted when no user row", seed: false, want: ""},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			riderID := "clrider-single-" + strconv.Itoa(i)
			if tt.seed {
				seedUser(t, riderID, tt.userName, tt.email)
			}

			rec := minimalRideRequest()
			rec.RiderID = riderID
			created, err := repo.Create(ctx, rec)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			// Create resolves the name inline (before insert).
			if created.RequesterName != tt.want {
				t.Errorf("Create RequesterName = %q, want %q", created.RequesterName, tt.want)
			}

			got, err := repo.GetByID(ctx, created.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if got.RequesterName != tt.want {
				t.Errorf("GetByID RequesterName = %q, want %q", got.RequesterName, tt.want)
			}
		})
	}
}

// TestRideRequestRepo_RequesterName_ListBatch verifies the list path resolves
// each row's requester independently via the inline requesterIdentitySelect
// subselect and omits the name for a rider with no "User" row — all within the
// single list query (no separate per-row or batched "User" lookup).
func TestRideRequestRepo_RequesterName_ListBatch(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	seedUser(t, "clrider-named", strPtr("Ada Lovelace"), strPtr("ada@example.com"))
	seedUser(t, "clrider-emailonly", nil, strPtr("grace@example.com"))
	// clrider-ghost intentionally has NO "User" row.

	want := map[string]string{
		"clrider-named":     "Ada",
		"clrider-emailonly": "grace",
		"clrider-ghost":     "",
	}

	owner := "clowner-list"
	for riderID := range want {
		rec := scheduledRideRequest() // scheduled: exempt from the one-active-instant guard
		rec.RiderID = riderID
		rec.OwnerID = owner
		if _, err := repo.Create(ctx, rec); err != nil {
			t.Fatalf("Create for %s: %v", riderID, err)
		}
	}

	page, err := repo.ListByOwnerPage(ctx, owner, nil, store.RideRequestListCursor{}, 50)
	if err != nil {
		t.Fatalf("ListByOwnerPage: %v", err)
	}
	if len(page.Items) != len(want) {
		t.Fatalf("got %d items, want %d", len(page.Items), len(want))
	}

	for _, item := range page.Items {
		if got := item.RequesterName; got != want[item.RiderID] {
			t.Errorf("rider %s RequesterName = %q, want %q", item.RiderID, got, want[item.RiderID])
		}
	}
}

// TestRideRequestRepo_RequesterName_OnStatusChange confirms the guarded
// transition return carries the requester name (the WS ride_status_changed
// frame is built from it).
func TestRideRequestRepo_RequesterName_OnStatusChange(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	riderID := "clrider-transition"
	seedUser(t, riderID, strPtr("Katherine Johnson"), strPtr("kj@example.com"))

	rec := minimalRideRequest()
	rec.RiderID = riderID
	created, err := repo.Create(ctx, rec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := repo.UpdateStatusFrom(ctx, created.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted)
	if err != nil {
		t.Fatalf("UpdateStatusFrom: %v", err)
	}
	if updated.RequesterName != "Katherine" {
		t.Errorf("UpdateStatusFrom RequesterName = %q, want %q", updated.RequesterName, "Katherine")
	}
}

// TestRideRequestRepo_RequesterName_MissingUserRowSucceeds is the fail-open
// guard (MYR-229): a ride operation must NEVER fail because the rider's "User"
// row is absent (deleted rider). Create and a guarded status transition both
// succeed and simply omit RequesterName (empty string).
func TestRideRequestRepo_RequesterName_MissingUserRowSucceeds(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	// No seedUser: the rider has no "User" row at all.
	rec := minimalRideRequest()
	rec.RiderID = "clrider-no-user-row"
	created, err := repo.Create(ctx, rec)
	if err != nil {
		t.Fatalf("Create must succeed with a missing User row: %v", err)
	}
	if created.RequesterName != "" {
		t.Errorf("Create RequesterName = %q, want %q (omitted)", created.RequesterName, "")
	}

	updated, err := repo.UpdateStatusFrom(ctx, created.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted)
	if err != nil {
		t.Fatalf("UpdateStatusFrom must succeed with a missing User row: %v", err)
	}
	if updated.RequesterName != "" {
		t.Errorf("UpdateStatusFrom RequesterName = %q, want %q (omitted)", updated.RequesterName, "")
	}
}
