package store_test

import (
	"context"
	"testing"
)

// MYR-413 — HasLiveActivity against a real database.
//
// The push-side matrix in internal/push proves what the notifier DOES with the
// answer, against a fake that returns whatever it is told. It cannot prove the
// answer itself, and the one distinction the whole feature turns on is a
// distinction only Postgres can make: a TOMBSTONED row and an ABSENT row are
// the same boolean to the caller and two very different rows on disk. A fake
// that returns false for both would keep passing if `ended_at IS NULL` were
// dropped from the predicate — which is precisely the change that would leave
// riders who swiped their card away with no notification at all.

// TestHasLiveActivity_TombstonedReadsAsAbsent is the central case.
func TestHasLiveActivity_TombstonedReadsAsAbsent(t *testing.T) {
	const ride = "cride0025p1"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()
	rider := "rider_" + ride

	if live, err := repo.HasLiveActivity(ctx, ride, rider); err != nil || live {
		t.Fatalf("HasLiveActivity before registration = (%v, %v), want (false, nil)", live, err)
	}

	if err := repo.RegisterActivity(ctx, ride, rider, "aa11bb22", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}
	live, err := repo.HasLiveActivity(ctx, ride, rider)
	if err != nil {
		t.Fatalf("HasLiveActivity after registration: %v", err)
	}
	if !live {
		t.Fatal("a freshly registered Activity reads as absent; every banner would " +
			"keep duplicating the island")
	}

	// The rider swipes the card away and the client ends the registration.
	if ended, err := repo.EndActivity(ctx, ride, rider); err != nil || !ended {
		t.Fatalf("EndActivity = (%v, %v), want (true, nil)", ended, err)
	}
	live, err = repo.HasLiveActivity(ctx, ride, rider)
	if err != nil {
		t.Fatalf("HasLiveActivity after end: %v", err)
	}
	if live {
		t.Error("a TOMBSTONED Activity reads as live — the rider swiped the card " +
			"away and would now be told nothing by either surface")
	}

	// Re-registering is the client saying it has a live Activity again, and the
	// upsert clears the tombstone. Suppression must come back with it.
	if err := repo.RegisterActivity(ctx, ride, rider, "cc33dd44", false); err != nil {
		t.Fatalf("re-RegisterActivity: %v", err)
	}
	if live, err := repo.HasLiveActivity(ctx, ride, rider); err != nil || !live {
		t.Errorf("HasLiveActivity after re-registration = (%v, %v), want (true, nil)", live, err)
	}
}

// TestHasLiveActivity_IsScopedToThePair proves the read cannot answer about the
// wrong person or the wrong ride.
//
// This is the store half of the rider-only scoping the notifier relies on: the
// owner's banner survives because the owner has no row, and nothing in the
// notifier says so — the KEY says so, and that claim is only true if this query
// filters on both columns.
func TestHasLiveActivity_IsScopedToThePair(t *testing.T) {
	const ride = "cride0025p2"
	const otherRide = "cride0025p3"
	repo := setupLiveActivities(t, ride)
	seedActivityRide(t, otherRide)
	ctx := context.Background()

	rider := "rider_" + ride
	if err := repo.RegisterActivity(ctx, ride, rider, "ee55ff66", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}

	tests := []struct {
		name   string
		rideID string
		userID string
		want   bool
	}{
		{name: "the registered pair", rideID: ride, userID: rider, want: true},
		{name: "the owner of the same ride", rideID: ride, userID: "cowner0025"},
		{name: "the same rider on another ride", rideID: otherRide, userID: rider},
		{name: "an unknown ride", rideID: "cride0025zz", userID: rider},
		{name: "an unknown user", rideID: ride, userID: "nobody"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repo.HasLiveActivity(ctx, tt.rideID, tt.userID)
			if err != nil {
				t.Fatalf("HasLiveActivity: %v", err)
			}
			if got != tt.want {
				t.Errorf("HasLiveActivity(%s, %s) = %v, want %v", tt.rideID, tt.userID, got, tt.want)
			}
		})
	}
}

// TestHasLiveActivity_RejectsEmptyIdentifiers keeps a blank argument from
// reading as a wildcard. An empty user id that returned some other row's answer
// would suppress a banner on the strength of somebody else's card.
func TestHasLiveActivity_RejectsEmptyIdentifiers(t *testing.T) {
	const ride = "cride0025p4"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	for _, tt := range []struct{ name, rideID, userID string }{
		{name: "no ride", rideID: "", userID: "rider_x"},
		{name: "no user", rideID: ride, userID: ""},
		{name: "neither", rideID: "", userID: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := repo.HasLiveActivity(ctx, tt.rideID, tt.userID); err == nil {
				t.Error("HasLiveActivity accepted an empty identifier")
			}
		})
	}
}
