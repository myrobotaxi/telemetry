package store_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Integration tests for SavedPlacesRepo (MYR-321). The encryption assertions
// are the point of the file: go_saved_places stores coordinates encrypt-only,
// so a regression that wrote them in the clear would be invisible to a
// round-trip test alone — several of these read the RAW COLUMN and check that
// what is on disk is NOT the coordinate.

const (
	savedPlaceUserA = "cusrsp0000000000000000a"
	savedPlaceUserB = "cusrsp0000000000000000b"
)

func newSavedPlacesRepo(t *testing.T) *store.SavedPlacesRepo {
	t.Helper()
	mustApplyGoMigrations(t)
	return store.NewSavedPlacesRepo(testPool, newTestEncryptor(t))
}

// rawSavedPlaceCiphertext reads the stored *_enc columns without decrypting.
func rawSavedPlaceCiphertext(t *testing.T, userID, kind string) (latEnc, lngEnc string) {
	t.Helper()
	err := testPool.QueryRow(context.Background(),
		`SELECT lat_enc, lng_enc FROM go_saved_places WHERE user_id = $1 AND kind = $2`,
		userID, kind).Scan(&latEnc, &lngEnc)
	if err != nil {
		t.Fatalf("read raw ciphertext: %v", err)
	}
	return latEnc, lngEnc
}

func homePlace() store.SavedPlace {
	return store.SavedPlace{
		Kind:      store.SavedPlaceHome,
		Label:     "1 Ferry Building · Embarcadero",
		Latitude:  37.7955,
		Longitude: -122.3937,
	}
}

// TestSavedPlacesRepo_EncryptionRoundTrip is the headline test: a coordinate
// written through the repo comes back EXACTLY, and what sits on disk is
// ciphertext that contains neither coordinate in any readable form.
func TestSavedPlacesRepo_EncryptionRoundTrip(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	want := homePlace()
	stored, err := repo.Upsert(ctx, savedPlaceUserA, want)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// The ECHO is scanned back out of the database through the decrypt path,
	// not reflected from the request — so an equal echo already proves one
	// full round trip.
	if stored != want {
		t.Fatalf("upsert echo = %+v, want %+v", stored, want)
	}

	// And a fresh read proves it again through the list path.
	places, err := repo.ListForUser(ctx, savedPlaceUserA)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(places) != 1 || places[0] != want {
		t.Fatalf("read back = %+v, want exactly [%+v]", places, want)
	}

	// EXACT, not approximate. The float codec is strconv.FormatFloat with
	// prec=-1, which is the shortest decimal that round-trips exactly — a
	// lossy %g would show up here as a coordinate metres from where it went in.
	if places[0].Latitude != want.Latitude || places[0].Longitude != want.Longitude {
		t.Fatalf("coordinates drifted: got (%v,%v) want (%v,%v)",
			places[0].Latitude, places[0].Longitude, want.Latitude, want.Longitude)
	}
}

// TestSavedPlacesRepo_CoordinatesAreNotStoredInTheClear reads the raw columns.
// A round-trip test alone would pass just as happily against a repo that never
// encrypted anything, so this is the assertion that actually pins NFR-3.23.
func TestSavedPlacesRepo_CoordinatesAreNotStoredInTheClear(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)

	place := homePlace()
	if _, err := repo.Upsert(context.Background(), savedPlaceUserA, place); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	latEnc, lngEnc := rawSavedPlaceCiphertext(t, savedPlaceUserA, "home")

	latPlain := strconv.FormatFloat(place.Latitude, 'g', -1, 64)
	lngPlain := strconv.FormatFloat(place.Longitude, 'g', -1, 64)

	if latEnc == latPlain || lngEnc == lngPlain {
		t.Fatal("coordinates were stored in the clear")
	}
	if strings.Contains(latEnc, latPlain) || strings.Contains(lngEnc, lngPlain) {
		t.Fatal("the plaintext coordinate is a substring of the stored ciphertext")
	}
	// Ciphertext is non-empty for a real value: the empty string is the
	// package's ABSENT sentinel, and an absent coordinate here would mean the
	// encrypt step silently no-opped.
	if latEnc == "" || lngEnc == "" {
		t.Fatal("empty ciphertext — the encrypt step no-opped")
	}
	// Nondeterministic: AES-GCM uses a fresh nonce per seal, so two encryptions
	// of the same value differ. Latitude and longitude here are different
	// values anyway, but equal ciphertext would mean the nonce was fixed.
	if latEnc == lngEnc {
		t.Fatal("lat and lng ciphertexts are identical")
	}
}

// TestSavedPlacesRepo_ADifferentKeyCannotRead proves the ciphertext is bound to
// the key rather than merely encoded: a repo holding a different KeySet fails
// the read instead of returning a coordinate.
func TestSavedPlacesRepo_ADifferentKeyCannotRead(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	if _, err := repo.Upsert(ctx, savedPlaceUserA, homePlace()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// A SECOND repo with an unrelated key.
	other := store.NewSavedPlacesRepo(testPool, newTestEncryptor(t))
	_, err := other.ListForUser(ctx, savedPlaceUserA)
	if err == nil {
		t.Fatal("a foreign key decrypted the coordinates, want a hard read error")
	}
	// A HARD ERROR, not a zero coordinate: there is no plaintext column to
	// fall back to, and surfacing (0,0) would route somebody to Null Island.
	if !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("error = %v, want a decrypt failure", err)
	}
}

// A nil Encryptor must fail at CONSTRUCTION, not at the first write — by then
// the deployment is live and the next request writes a home address in the
// clear.
func TestSavedPlacesRepo_NilEncryptorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil encryptor")
		}
	}()
	_ = store.NewSavedPlacesRepo(testPool, nil)
}

// An account with no rows gets an EMPTY, NON-NIL slice and no error. That is
// the state every account starts in, so the no-rows branch is the common path.
func TestSavedPlacesRepo_EmptyAccountReturnsEmptyNonNilSlice(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)

	places, err := repo.ListForUser(context.Background(), savedPlaceUserA)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if places == nil {
		t.Fatal("returned a nil slice; the wire projection must render [] not null")
	}
	if len(places) != 0 {
		t.Fatalf("places = %+v, want empty", places)
	}
}

// Both kinds, home first — the ORDER BY kind the list query declares.
func TestSavedPlacesRepo_ListReturnsHomeBeforeWork(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	// Insert WORK FIRST so a passing test cannot be insertion order.
	work := store.SavedPlace{Kind: store.SavedPlaceWork, Label: "HQ", Latitude: 37.3947, Longitude: -122.1503}
	if _, err := repo.Upsert(ctx, savedPlaceUserA, work); err != nil {
		t.Fatalf("Upsert work: %v", err)
	}
	if _, err := repo.Upsert(ctx, savedPlaceUserA, homePlace()); err != nil {
		t.Fatalf("Upsert home: %v", err)
	}

	places, err := repo.ListForUser(ctx, savedPlaceUserA)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(places) != 2 {
		t.Fatalf("places = %d, want 2", len(places))
	}
	if places[0].Kind != store.SavedPlaceHome || places[1].Kind != store.SavedPlaceWork {
		t.Fatalf("order = %s,%s — want home,work", places[0].Kind, places[1].Kind)
	}
}

// The upsert REPLACES the whole slot rather than merging, and it never mints a
// second row for the same kind — the composite primary key is the arbiter.
func TestSavedPlacesRepo_UpsertReplacesWholeSlot(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	if _, err := repo.Upsert(ctx, savedPlaceUserA, homePlace()); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	moved := store.SavedPlace{
		Kind:      store.SavedPlaceHome,
		Label:     "New House",
		Latitude:  40.7128,
		Longitude: -74.0060,
	}
	stored, err := repo.Upsert(ctx, savedPlaceUserA, moved)
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if stored != moved {
		t.Fatalf("echo = %+v, want %+v", stored, moved)
	}

	places, err := repo.ListForUser(ctx, savedPlaceUserA)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(places) != 1 {
		t.Fatalf("places = %d, want 1 — the upsert minted a second home", len(places))
	}
	// The OLD label must be gone. A merge would have kept "1 Ferry Building"
	// against the new coordinates, which is the exact failure the whole-object
	// write exists to prevent.
	if places[0].Label != "New House" {
		t.Fatalf("label = %q, want the replacement", places[0].Label)
	}
}

// created_at survives a replace; updated_at moves. Replacing the address does
// not make "when this person first saved a home" a new fact.
func TestSavedPlacesRepo_UpsertPreservesCreatedAt(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	if _, err := repo.Upsert(ctx, savedPlaceUserA, homePlace()); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	var created1, updated1 string
	if err := testPool.QueryRow(ctx,
		`SELECT created_at::text, updated_at::text FROM go_saved_places WHERE user_id=$1 AND kind='home'`,
		savedPlaceUserA).Scan(&created1, &updated1); err != nil {
		t.Fatalf("read timestamps: %v", err)
	}

	moved := homePlace()
	moved.Latitude = 40
	if _, err := repo.Upsert(ctx, savedPlaceUserA, moved); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	var created2, updated2 string
	if err := testPool.QueryRow(ctx,
		`SELECT created_at::text, updated_at::text FROM go_saved_places WHERE user_id=$1 AND kind='home'`,
		savedPlaceUserA).Scan(&created2, &updated2); err != nil {
		t.Fatalf("read timestamps: %v", err)
	}

	if created1 != created2 {
		t.Errorf("created_at moved on replace: %s -> %s", created1, created2)
	}
	if updated1 == updated2 {
		t.Errorf("updated_at did not move on replace: %s", updated2)
	}
}

// One person's places are invisible to another. Sharing a car grants access to
// the CAR, never to the other person's address book.
func TestSavedPlacesRepo_PlacesAreScopedToOneAccount(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	if _, err := repo.Upsert(ctx, savedPlaceUserA, homePlace()); err != nil {
		t.Fatalf("Upsert A: %v", err)
	}

	places, err := repo.ListForUser(ctx, savedPlaceUserB)
	if err != nil {
		t.Fatalf("ListForUser B: %v", err)
	}
	if len(places) != 0 {
		t.Fatalf("user B can see user A's places: %+v", places)
	}
}

// Delete reports whether a row went, is idempotent, and touches only the named
// slot.
func TestSavedPlacesRepo_DeleteIsScopedAndIdempotent(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	if _, err := repo.Upsert(ctx, savedPlaceUserA, homePlace()); err != nil {
		t.Fatalf("Upsert home: %v", err)
	}
	work := store.SavedPlace{Kind: store.SavedPlaceWork, Label: "HQ", Latitude: 1, Longitude: 2}
	if _, err := repo.Upsert(ctx, savedPlaceUserA, work); err != nil {
		t.Fatalf("Upsert work: %v", err)
	}

	removed, err := repo.Delete(ctx, savedPlaceUserA, store.SavedPlaceHome)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !removed {
		t.Fatal("Delete reported no row removed")
	}

	// Idempotent: a second delete removes nothing and is NOT an error.
	removed, err = repo.Delete(ctx, savedPlaceUserA, store.SavedPlaceHome)
	if err != nil {
		t.Fatalf("re-Delete: %v", err)
	}
	if removed {
		t.Fatal("re-Delete reported a row removed")
	}

	// Work survived.
	places, err := repo.ListForUser(ctx, savedPlaceUserA)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(places) != 1 || places[0].Kind != store.SavedPlaceWork {
		t.Fatalf("places = %+v, want only work", places)
	}
}

// The repo rejects a bad kind before it can reach the CHECK constraint, so the
// endpoint answers 400 rather than 500 if validation is ever bypassed.
func TestSavedPlacesRepo_RejectsInvalidInputBeforeSQL(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		run    func() error
		expect string
	}{
		{
			name:   "upsert with an unknown kind",
			run:    func() error { _, e := repo.Upsert(ctx, savedPlaceUserA, store.SavedPlace{Kind: "gym"}); return e },
			expect: "invalid kind",
		},
		{
			name:   "upsert with a mis-cased kind",
			run:    func() error { _, e := repo.Upsert(ctx, savedPlaceUserA, store.SavedPlace{Kind: "Home"}); return e },
			expect: "invalid kind",
		},
		{
			name:   "delete with an unknown kind",
			run:    func() error { _, e := repo.Delete(ctx, savedPlaceUserA, "gym"); return e },
			expect: "invalid kind",
		},
		{
			name:   "upsert with an empty user id",
			run:    func() error { _, e := repo.Upsert(ctx, "  ", homePlace()); return e },
			expect: "empty user id",
		},
		{
			name:   "list with an empty user id",
			run:    func() error { _, e := repo.ListForUser(ctx, ""); return e },
			expect: "empty user id",
		},
		{
			name:   "delete with an empty user id",
			run:    func() error { _, e := repo.Delete(ctx, "", store.SavedPlaceHome); return e },
			expect: "empty user id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("want an error containing %q", tc.expect)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.expect)
			}
		})
	}
}

// Coordinates that are awkward for a float codec must survive exactly:
// negatives, high precision, and the zeroes that a naive "is it set?" check
// would treat as absent.
func TestSavedPlacesRepo_CoordinateEdgeValuesRoundTripExactly(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		lat, lng float64
	}{
		{"null island", 0, 0},
		{"north pole", 90, 0},
		{"south pole", -90, 0},
		{"antimeridian east", 0, 180},
		{"antimeridian west", 0, -180},
		{"high precision", 37.795512345678901, -122.393712345678901},
		{"tiny magnitude", 0.000001, -0.000001},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cleanSavedPlaces(t)
			want := store.SavedPlace{
				Kind:      store.SavedPlaceHome,
				Label:     "Edge",
				Latitude:  tc.lat,
				Longitude: tc.lng,
			}
			if _, err := repo.Upsert(ctx, savedPlaceUserA, want); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			places, err := repo.ListForUser(ctx, savedPlaceUserA)
			if err != nil {
				t.Fatalf("ListForUser: %v", err)
			}
			if len(places) != 1 || places[0] != want {
				t.Fatalf("round trip = %+v, want [%+v]", places, want)
			}
		})
	}
}

// A tampered ciphertext fails the read rather than surfacing a coordinate —
// the GCM auth tag is what makes that a guarantee and not a hope.
func TestSavedPlacesRepo_TamperedCiphertextFailsTheRead(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	if _, err := repo.Upsert(ctx, savedPlaceUserA, homePlace()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE go_saved_places SET lat_enc = 'not-a-ciphertext' WHERE user_id=$1 AND kind='home'`,
		savedPlaceUserA); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	if _, err := repo.ListForUser(ctx, savedPlaceUserA); err == nil {
		t.Fatal("a corrupt ciphertext read cleanly, want a hard error")
	}
}

// The account-deletion sweep removes BOTH slots and is idempotent — the
// property that makes the MYR-355 sequence re-runnable.
func TestAccountDeleter_DeleteSavedPlacesRemovesBothKinds(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	repo := newSavedPlacesRepo(t)
	cleanSavedPlaces(t)
	ctx := context.Background()

	if _, err := repo.Upsert(ctx, savedPlaceUserA, homePlace()); err != nil {
		t.Fatalf("Upsert home: %v", err)
	}
	work := store.SavedPlace{Kind: store.SavedPlaceWork, Label: "HQ", Latitude: 1, Longitude: 2}
	if _, err := repo.Upsert(ctx, savedPlaceUserA, work); err != nil {
		t.Fatalf("Upsert work: %v", err)
	}
	// A second account's place must survive the sweep.
	if _, err := repo.Upsert(ctx, savedPlaceUserB, homePlace()); err != nil {
		t.Fatalf("Upsert B: %v", err)
	}

	deleter := store.NewAccountDeleter(testPool, testLogger())

	n, err := deleter.DeleteSavedPlaces(ctx, savedPlaceUserA)
	if err != nil {
		t.Fatalf("DeleteSavedPlaces: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted = %d, want 2", n)
	}

	// Idempotent: a re-run deletes nothing and does not error.
	n, err = deleter.DeleteSavedPlaces(ctx, savedPlaceUserA)
	if err != nil {
		t.Fatalf("re-run DeleteSavedPlaces: %v", err)
	}
	if n != 0 {
		t.Fatalf("re-run deleted = %d, want 0", n)
	}

	// Scoped: the other account kept its place.
	places, err := repo.ListForUser(ctx, savedPlaceUserB)
	if err != nil {
		t.Fatalf("ListForUser B: %v", err)
	}
	if len(places) != 1 {
		t.Fatalf("the sweep took another account's places: %+v", places)
	}

	// A REAL DELETE, not a tombstone: no row of any status survives, so the
	// ciphertext of where this person lives does not outlive their account.
	var remaining int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM go_saved_places WHERE user_id = $1`, savedPlaceUserA).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("%d rows survived the account deletion", remaining)
	}
}

// An empty user id must not become a table-wide DELETE.
func TestAccountDeleter_DeleteSavedPlacesRejectsEmptyUser(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	deleter := store.NewAccountDeleter(testPool, testLogger())

	if _, err := deleter.DeleteSavedPlaces(context.Background(), "   "); err == nil {
		t.Fatal("an empty user id was accepted")
	}
}
