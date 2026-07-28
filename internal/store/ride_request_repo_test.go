package store_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// setupRideRequestRepo applies the embedded migrations (idempotent — the
// go_ride_requests table arrives with 0002), truncates the table, and
// returns a repo wired with a fresh random-key encryptor. Each test owns
// its data (CLAUDE.md "No test pollution").
func setupRideRequestRepo(t *testing.T) (*store.RideRequestRepo, cryptox.Encryptor) {
	t.Helper()
	if !dockerAvailable {
		t.Skip("Docker not available -- skipping ride-request integration test")
	}
	ctx := context.Background()
	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
		t.Fatalf("clean go_ride_requests: %v", err)
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM "User"`); err != nil {
		t.Fatalf("clean User: %v", err)
	}
	// MYR-264 — the requester-identity join now also reads the Apple-native
	// identity tables; clean them too so a seeded rider never leaks across tests.
	for _, tbl := range []string{`go_identity_apple`, `go_users`} {
		if _, err := testPool.Exec(ctx, `DELETE FROM `+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
	enc := newTestEncryptor(t)
	return store.NewRideRequestRepo(testPool, store.NoopMetrics{}, enc), enc
}

// fullRideRequest is a scheduled, booked-for-someone-else request — every
// optional Create field populated.
func fullRideRequest() store.RideRequestRecord {
	sched := time.Date(2026, 6, 18, 16, 0, 0, 0, time.UTC)
	return store.RideRequestRecord{
		RiderID:   "clrider1234567890abcdef",
		OwnerID:   "clowner1234567890abcdef",
		VehicleID: "clxyz1234567890abcdef",
		Pickup: store.RidePlace{
			Latitude: 37.7955, Longitude: -122.3937,
			Label: "Home", Address: strPtr("221 Folsom St, San Francisco"),
		},
		Dropoff: store.RidePlace{
			Latitude: 37.7766, Longitude: -122.3946,
			Label: "Caltrain · 4th & King",
		},
		PassengerName:  strPtr("Maya Chen"),
		PassengerPhone: strPtr("(415) 555-0142"),
		ScheduledFor:   &sched,
	}
}

// minimalRideRequest is an on-demand ride for the rider themselves.
func minimalRideRequest() store.RideRequestRecord {
	return store.RideRequestRecord{
		RiderID:   "clrider1234567890abcdef",
		OwnerID:   "clowner1234567890abcdef",
		VehicleID: "clxyz1234567890abcdef",
		Pickup: store.RidePlace{
			Latitude: 37.7749, Longitude: -122.4194, Label: "Current location",
		},
		Dropoff: store.RidePlace{
			Latitude: 37.7599, Longitude: -122.4148, Label: "Tartine Bakery",
		},
	}
}

// scheduledRideRequest is minimalRideRequest with a fixed future scheduledFor.
// Scheduled rides are EXEMPT from the one-active-INSTANT-ride guard (MYR-230,
// migration 0004's partial unique index only covers scheduled_for IS NULL
// rows), so tests that need several concurrently-open rides for a single rider
// build them as scheduled to stay in a state the business rule permits.
func scheduledRideRequest() store.RideRequestRecord {
	rec := minimalRideRequest()
	sched := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	rec.ScheduledFor = &sched
	return rec
}

// TestRideRequestMigration_TableApplied proves migration 0002 lands the
// table (and that re-running stays idempotent — RunMigrations is invoked
// again by every other test's setup).
func TestRideRequestMigration_TableApplied(t *testing.T) {
	_, _ = setupRideRequestRepo(t)
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM go_ride_requests`).Scan(&n); err != nil {
		t.Fatalf("go_ride_requests not queryable after migrations: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected empty table, got %d rows", n)
	}
}

func TestRideRequestRepo_CreateAndGetRoundTrip(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	tests := []struct {
		name string
		in   store.RideRequestRecord
	}{
		{name: "full scheduled booked-for request", in: fullRideRequest()},
		{name: "minimal on-demand request", in: minimalRideRequest()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			created, err := repo.Create(ctx, tt.in)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if created.ID == "" || !strings.HasPrefix(created.ID, "c") {
				t.Errorf("expected generated cuid-shaped id, got %q", created.ID)
			}
			if created.Status != store.RideRequestStatusRequested {
				t.Errorf("expected default status 'requested', got %q", created.Status)
			}
			if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
				t.Error("expected DB-assigned created_at/updated_at")
			}

			got, err := repo.GetByID(ctx, created.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if got.Pickup.Latitude != tt.in.Pickup.Latitude || got.Pickup.Longitude != tt.in.Pickup.Longitude {
				t.Errorf("pickup coords: got (%v,%v) want (%v,%v)",
					got.Pickup.Latitude, got.Pickup.Longitude, tt.in.Pickup.Latitude, tt.in.Pickup.Longitude)
			}
			if got.Dropoff.Latitude != tt.in.Dropoff.Latitude || got.Dropoff.Longitude != tt.in.Dropoff.Longitude {
				t.Errorf("dropoff coords: got (%v,%v) want (%v,%v)",
					got.Dropoff.Latitude, got.Dropoff.Longitude, tt.in.Dropoff.Latitude, tt.in.Dropoff.Longitude)
			}
			if got.Pickup.Label != tt.in.Pickup.Label || got.Dropoff.Label != tt.in.Dropoff.Label {
				t.Errorf("labels: got (%q,%q) want (%q,%q)",
					got.Pickup.Label, got.Dropoff.Label, tt.in.Pickup.Label, tt.in.Dropoff.Label)
			}
			assertPtrEq(t, "pickup address", got.Pickup.Address, tt.in.Pickup.Address)
			assertPtrEq(t, "passenger name", got.PassengerName, tt.in.PassengerName)
			assertPtrEq(t, "passenger phone", got.PassengerPhone, tt.in.PassengerPhone)
			if (got.ScheduledFor == nil) != (tt.in.ScheduledFor == nil) {
				t.Errorf("scheduledFor presence mismatch: got %v want %v", got.ScheduledFor, tt.in.ScheduledFor)
			}
			if got.ScheduledFor != nil && !got.ScheduledFor.Equal(*tt.in.ScheduledFor) {
				t.Errorf("scheduledFor: got %v want %v", got.ScheduledFor, tt.in.ScheduledFor)
			}
			if got.AcceptedAt != nil || got.CompletedAt != nil {
				t.Error("fresh request must not carry accepted_at/completed_at")
			}
			if got.RescheduleStatus != nil || got.RescheduleProposedFor != nil {
				t.Error("fresh request must not carry a reschedule sub-state")
			}
		})
	}
}

// TestRideRequestRepo_CoordinatesStoredEncrypted proves the encrypt-only
// contract (NFR-3.23): raw *_enc columns never contain the plaintext
// coordinate string.
func TestRideRequestRepo_CoordinatesStoredEncrypted(t *testing.T) {
	repo, enc := setupRideRequestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, minimalRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var pickupLatEnc, dropoffLngEnc string
	err = testPool.QueryRow(ctx,
		`SELECT pickup_lat_enc, dropoff_lng_enc FROM go_ride_requests WHERE id = $1`,
		created.ID).Scan(&pickupLatEnc, &dropoffLngEnc)
	if err != nil {
		t.Fatalf("raw select: %v", err)
	}

	latPlain := strconv.FormatFloat(37.7749, 'g', -1, 64)
	if pickupLatEnc == latPlain || strings.Contains(pickupLatEnc, latPlain) {
		t.Errorf("pickup_lat_enc %q looks like plaintext", pickupLatEnc)
	}
	// Ciphertext decrypts back to the exact shortest-round-trip string.
	got, err := enc.DecryptString(pickupLatEnc)
	if err != nil {
		t.Fatalf("DecryptString(pickup_lat_enc): %v", err)
	}
	if got != latPlain {
		t.Errorf("decrypted pickup lat = %q, want %q", got, latPlain)
	}
	lngPlain := strconv.FormatFloat(-122.4148, 'g', -1, 64)
	if dropoffLngEnc == lngPlain || strings.Contains(dropoffLngEnc, lngPlain) {
		t.Errorf("dropoff_lng_enc %q looks like plaintext", dropoffLngEnc)
	}
}

func TestRideRequestRepo_GetByID_NotFound(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	_, err := repo.GetByID(context.Background(), "cnope")
	if !errors.Is(err, store.ErrRideRequestNotFound) {
		t.Fatalf("expected ErrRideRequestNotFound, got %v", err)
	}
	if !errors.Is(err, sdk.ErrNotFound) {
		t.Fatalf("expected error to wrap sdk.ErrNotFound, got %v", err)
	}
}

func TestRideRequestRepo_Lists(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	mk := func(mut func(*store.RideRequestRecord)) store.RideRequestRecord {
		rec := minimalRideRequest()
		mut(&rec)
		created, err := repo.Create(ctx, rec)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return created
	}

	r1 := mk(func(r *store.RideRequestRecord) {}) // rider A -> owner A (open instant)
	// r2 is rider A's second row: scheduled, so it does not collide with r1
	// under the one-active-instant-ride guard (MYR-230).
	r2 := mk(func(r *store.RideRequestRecord) {
		sched := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		r.ScheduledFor = &sched
	})
	r3 := mk(func(r *store.RideRequestRecord) { r.RiderID = "clriderB000000000000000" })
	if _, err := repo.UpdateStatus(ctx, r2.ID, store.RideRequestStatusAccepted); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	t.Run("ListByRider returns only that rider's rows, newest first", func(t *testing.T) {
		got, err := repo.ListByRider(ctx, r1.RiderID, 0)
		if err != nil {
			t.Fatalf("ListByRider: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 rows, got %d", len(got))
		}
		// r1 and r2 were created in the same NOW() neighborhood; ordering is
		// (created_at DESC, id DESC) — assert both present and no r3.
		ids := map[string]bool{got[0].ID: true, got[1].ID: true}
		if !ids[r1.ID] || !ids[r2.ID] {
			t.Errorf("expected rows %s and %s, got %v", r1.ID, r2.ID, ids)
		}
		if !got[0].CreatedAt.Before(time.Now().Add(time.Hour)) {
			t.Error("sanity: created_at in the future")
		}
	})

	t.Run("ListByRider honors limit", func(t *testing.T) {
		got, err := repo.ListByRider(ctx, r1.RiderID, 1)
		if err != nil {
			t.Fatalf("ListByRider: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 row with limit 1, got %d", len(got))
		}
	})

	t.Run("ListByOwner without status filter returns all", func(t *testing.T) {
		got, err := repo.ListByOwner(ctx, r1.OwnerID, nil, 0)
		if err != nil {
			t.Fatalf("ListByOwner: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("expected 3 rows (incl. rider B's), got %d", len(got))
		}
	})

	t.Run("ListByOwner filters by status", func(t *testing.T) {
		status := store.RideRequestStatusRequested
		got, err := repo.ListByOwner(ctx, r1.OwnerID, &status, 0)
		if err != nil {
			t.Fatalf("ListByOwner: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 'requested' rows, got %d", len(got))
		}
		for _, rec := range got {
			if rec.ID == r2.ID {
				t.Error("accepted row r2 must be filtered out")
			}
		}
	})

	t.Run("empty result is an empty slice, not nil", func(t *testing.T) {
		got, err := repo.ListByRider(ctx, "clnobody", 0)
		if err != nil {
			t.Fatalf("ListByRider: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("expected empty non-nil slice, got %#v", got)
		}
	})

	_ = r3
}

func TestRideRequestRepo_UpdateStatus(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	tests := []struct {
		name          string
		transitions   []store.RideRequestStatus
		wantAccepted  bool
		wantCompleted bool
	}{
		{
			name:        "accepted stamps accepted_at only",
			transitions: []store.RideRequestStatus{store.RideRequestStatusAccepted},
			wantAccepted: true,
		},
		{
			name:          "full lifecycle stamps both",
			transitions:   []store.RideRequestStatus{store.RideRequestStatusAccepted, store.RideRequestStatusEnroute, store.RideRequestStatusArrived, store.RideRequestStatusCompleted},
			wantAccepted:  true,
			wantCompleted: true,
		},
		{
			name:        "declined stamps neither",
			transitions: []store.RideRequestStatus{store.RideRequestStatusDeclined},
		},
		{
			name:        "cancelled stamps neither",
			transitions: []store.RideRequestStatus{store.RideRequestStatusCancelled},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Scheduled so subtests sharing this repo don't collide under the
			// one-active-instant-ride guard (MYR-230) — one subtest leaves the
			// ride in an open 'accepted' state, which would otherwise block the
			// next subtest's create.
			created, err := repo.Create(ctx, scheduledRideRequest())
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			var rec store.RideRequestRecord
			for _, s := range tt.transitions {
				rec, err = repo.UpdateStatus(ctx, created.ID, s)
				if err != nil {
					t.Fatalf("UpdateStatus(%s): %v", s, err)
				}
			}
			if rec.Status != tt.transitions[len(tt.transitions)-1] {
				t.Errorf("status = %q, want %q", rec.Status, tt.transitions[len(tt.transitions)-1])
			}
			if (rec.AcceptedAt != nil) != tt.wantAccepted {
				t.Errorf("accepted_at set = %v, want %v", rec.AcceptedAt != nil, tt.wantAccepted)
			}
			if (rec.CompletedAt != nil) != tt.wantCompleted {
				t.Errorf("completed_at set = %v, want %v", rec.CompletedAt != nil, tt.wantCompleted)
			}
			if tt.wantAccepted && rec.AcceptedAt.Before(rec.CreatedAt) {
				t.Errorf("accepted_at %v precedes created_at %v", rec.AcceptedAt, rec.CreatedAt)
			}
			if tt.wantCompleted && rec.CompletedAt.Before(*rec.AcceptedAt) {
				t.Errorf("completed_at %v precedes accepted_at %v", rec.CompletedAt, rec.AcceptedAt)
			}
			if !rec.UpdatedAt.After(rec.CreatedAt) && !rec.UpdatedAt.Equal(rec.CreatedAt) {
				t.Errorf("updated_at %v not >= created_at %v", rec.UpdatedAt, rec.CreatedAt)
			}
		})
	}

	t.Run("re-entering accepted never moves the original stamp", func(t *testing.T) {
		created, err := repo.Create(ctx, scheduledRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		first, err := repo.UpdateStatus(ctx, created.ID, store.RideRequestStatusAccepted)
		if err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}
		again, err := repo.UpdateStatus(ctx, created.ID, store.RideRequestStatusAccepted)
		if err != nil {
			t.Fatalf("UpdateStatus (again): %v", err)
		}
		if !again.AcceptedAt.Equal(*first.AcceptedAt) {
			t.Errorf("accepted_at moved on re-accept: %v -> %v", first.AcceptedAt, again.AcceptedAt)
		}
	})

	t.Run("unknown id returns ErrRideRequestNotFound", func(t *testing.T) {
		_, err := repo.UpdateStatus(ctx, "cnope", store.RideRequestStatusAccepted)
		if !errors.Is(err, store.ErrRideRequestNotFound) {
			t.Fatalf("expected ErrRideRequestNotFound, got %v", err)
		}
	})

	t.Run("status outside the contract enum is rejected by the CHECK", func(t *testing.T) {
		created, err := repo.Create(ctx, scheduledRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := repo.UpdateStatus(ctx, created.ID, "hovering"); err == nil {
			t.Fatal("expected CHECK-constraint error for unknown status, got nil")
		}
	})
}

func TestRideRequestRepo_Reschedule(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	proposed := time.Date(2026, 6, 19, 16, 30, 0, 0, time.UTC)

	newAcceptedScheduled := func(t *testing.T) store.RideRequestRecord {
		t.Helper()
		created, err := repo.Create(ctx, fullRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		accepted, err := repo.UpdateStatus(ctx, created.ID, store.RideRequestStatusAccepted)
		if err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}
		return accepted
	}

	t.Run("propose opens the negotiation without touching the main status", func(t *testing.T) {
		ride := newAcceptedScheduled(t)
		rec, err := repo.ProposeReschedule(ctx, ride.ID, proposed)
		if err != nil {
			t.Fatalf("ProposeReschedule: %v", err)
		}
		if rec.Status != store.RideRequestStatusAccepted {
			t.Errorf("main status changed to %q; reschedule must be orthogonal", rec.Status)
		}
		if rec.RescheduleStatus == nil || *rec.RescheduleStatus != store.RescheduleStatusRequested {
			t.Errorf("reschedule_status = %v, want 'requested'", rec.RescheduleStatus)
		}
		if rec.RescheduleProposedFor == nil || !rec.RescheduleProposedFor.Equal(proposed) {
			t.Errorf("reschedule_proposed_for = %v, want %v", rec.RescheduleProposedFor, proposed)
		}
		if !rec.ScheduledFor.Equal(*ride.ScheduledFor) {
			t.Errorf("scheduled_for moved on propose: %v -> %v", ride.ScheduledFor, rec.ScheduledFor)
		}
	})

	t.Run("confirm adopts the proposed time into scheduled_for", func(t *testing.T) {
		ride := newAcceptedScheduled(t)
		if _, err := repo.ProposeReschedule(ctx, ride.ID, proposed); err != nil {
			t.Fatalf("ProposeReschedule: %v", err)
		}
		rec, err := repo.ResolveReschedule(ctx, ride.ID, true)
		if err != nil {
			t.Fatalf("ResolveReschedule(confirm): %v", err)
		}
		if rec.ScheduledFor == nil || !rec.ScheduledFor.Equal(proposed) {
			t.Errorf("scheduled_for = %v, want confirmed time %v", rec.ScheduledFor, proposed)
		}
		if rec.RescheduleStatus == nil || *rec.RescheduleStatus != store.RescheduleStatusConfirmed {
			t.Errorf("reschedule_status = %v, want 'confirmed'", rec.RescheduleStatus)
		}
		if rec.RescheduleProposedFor == nil {
			t.Error("proposed time must be retained for audit after confirm")
		}
	})

	t.Run("decline keeps the original reservation", func(t *testing.T) {
		ride := newAcceptedScheduled(t)
		if _, err := repo.ProposeReschedule(ctx, ride.ID, proposed); err != nil {
			t.Fatalf("ProposeReschedule: %v", err)
		}
		rec, err := repo.ResolveReschedule(ctx, ride.ID, false)
		if err != nil {
			t.Fatalf("ResolveReschedule(decline): %v", err)
		}
		if !rec.ScheduledFor.Equal(*ride.ScheduledFor) {
			t.Errorf("scheduled_for = %v, want original %v", rec.ScheduledFor, ride.ScheduledFor)
		}
		if rec.RescheduleStatus == nil || *rec.RescheduleStatus != store.RescheduleStatusDeclined {
			t.Errorf("reschedule_status = %v, want 'declined'", rec.RescheduleStatus)
		}
	})

	t.Run("resolving without an open negotiation is not found", func(t *testing.T) {
		ride := newAcceptedScheduled(t)
		if _, err := repo.ResolveReschedule(ctx, ride.ID, true); !errors.Is(err, store.ErrRideRequestNotFound) {
			t.Fatalf("expected ErrRideRequestNotFound for never-proposed ride, got %v", err)
		}
		if _, err := repo.ProposeReschedule(ctx, ride.ID, proposed); err != nil {
			t.Fatalf("ProposeReschedule: %v", err)
		}
		if _, err := repo.ResolveReschedule(ctx, ride.ID, true); err != nil {
			t.Fatalf("ResolveReschedule: %v", err)
		}
		if _, err := repo.ResolveReschedule(ctx, ride.ID, true); !errors.Is(err, store.ErrRideRequestNotFound) {
			t.Fatalf("expected ErrRideRequestNotFound on double-resolve, got %v", err)
		}
	})
}

// assertPtrEq compares two optional strings by value.
func assertPtrEq(t *testing.T, field string, got, want *string) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s: got %v want %v", field, got, want)
	case *got != *want:
		t.Errorf("%s: got %q want %q", field, *got, *want)
	}
}
