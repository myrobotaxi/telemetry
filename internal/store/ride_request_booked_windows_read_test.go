// Integration coverage for the MYR-385 picker read surface, against a real
// Postgres. Filename note: `_read_test.go`, never `_windows_test.go` — Go
// strips `_test` and then reads `windows` as a GOOS build constraint, and the
// whole file would vanish from a darwin/linux build without an error.
//
// What these tests are FOR. The surface's only real claim is an equivalence:
// an interval it returns is exactly an interval the MYR-383 gate would refuse,
// and an instant outside every returned interval is exactly one the gate would
// allow. Row-shape assertions are the cheap half of that; the two tests that
// actually pin it are TestBookedWindows_WidthTracksTheConflictConstant (the
// width is DERIVED from store.RideConflictWindow, so moving the const moves the
// picker) and TestBookedWindows_EmittedEdgeIsBookable (the edges are the gate's
// own boundary, proved by booking against them in both directions).

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// windowsBase anchors every reservation in this file. Far enough from `now`
// that the ACTIVE-INSTANT arm can never fire by accident — the one test that
// wants that arm builds its ride from time.Now() explicitly.
var windowsBase = time.Date(2029, 6, 12, 15, 0, 0, 0, time.UTC)

const (
	windowsVehicle = "clxyz1234567890abcdef" // minimalRideRequest's vehicle
	windowsRider   = "clrider1234567890abcdef"
	windowsOther   = "clother234567890abcdef"
)

// listWindows reads the surface over a range generous enough to hold every
// fixture in a test, unless the test is specifically about range filtering.
func listWindows(t *testing.T, repo *store.RideRequestRepo, callerID string, from, to time.Time) []store.BookedWindow {
	t.Helper()
	got, err := repo.ListBookedWindows(context.Background(), windowsVehicle, callerID, from, to)
	if err != nil {
		t.Fatalf("ListBookedWindows: %v", err)
	}
	return got
}

// setStatusDirect forces a ride's status with raw SQL. Deliberately NOT routed
// through the repo's guarded transitions: this file is testing which rows the
// READ predicate admits, and reaching `completed` through the real lifecycle
// would take three writes whose legality is another file's subject.
func setStatusDirect(t *testing.T, id, status string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_requests SET status = $2 WHERE id = $1`, id, status); err != nil {
		t.Fatalf("force status %s on %s: %v", status, id, err)
	}
}

// bookAs creates a reservation at `at` for a named rider, so the `own` flag has
// two sides to distinguish.
func bookAs(t *testing.T, repo *store.RideRequestRepo, riderID string, at time.Time) store.RideRequestRecord {
	t.Helper()
	rec := minimalRideRequest()
	rec.RiderID = riderID
	rec.ScheduledFor = &at
	created, err := repo.Create(context.Background(), rec)
	if err != nil {
		t.Fatalf("Create(rider=%s, %s): %v", riderID, at.Format(time.RFC3339), err)
	}
	return created
}

// TestBookedWindows_EmitsRequestedAndAccepted is the CREATE-path semantics the
// picker must inherit: a still-undecided `requested` reservation occupies its
// window exactly as hard as a committed one, and is only labelled differently.
//
// This is the arm that closes the reported defect. A picker built on the
// ACCEPT-path answer would leave the pending slot enabled and hand the rider
// straight back into the 409 this endpoint exists to prevent.
func TestBookedWindows_EmitsRequestedAndAccepted(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)

	// The 15:00 claim is left at `requested`; the 19:00 one is accepted.
	bookAs(t, repo, windowsRider, windowsBase)
	committed := bookAs(t, repo, windowsRider, windowsBase.Add(4*time.Hour))
	mustAccept(t, repo, committed.ID)

	got := listWindows(t, repo, windowsRider, windowsBase.Add(-6*time.Hour), windowsBase.Add(10*time.Hour))
	if len(got) != 2 {
		t.Fatalf("expected 2 windows, got %d: %+v", len(got), got)
	}

	// Ordered by start, so [0] is the pending 15:00 claim and [1] the accepted
	// 19:00 one.
	if !got[0].Pending {
		t.Errorf("window[0] (a `requested` reservation) should be pending=true, got %+v", got[0])
	}
	if got[1].Pending {
		t.Errorf("window[1] (an `accepted` reservation) should be pending=false, got %+v", got[1])
	}
	if !got[0].Start.Equal(windowsBase.Add(-store.RideConflictWindow)) {
		t.Errorf("window[0].Start = %s, want %s",
			got[0].Start.UTC(), windowsBase.Add(-store.RideConflictWindow).UTC())
	}
	if !got[1].End.Equal(windowsBase.Add(4*time.Hour + store.RideConflictWindow)) {
		t.Errorf("window[1].End = %s, want %s",
			got[1].End.UTC(), windowsBase.Add(4*time.Hour+store.RideConflictWindow).UTC())
	}
}

// TestBookedWindows_ExcludesTerminalRides pins the DEFERRAL property from the
// read side: a refusal is never a permanent hold on a slot, so a terminal ride
// must stop dimming its window the instant it reaches that state. A picker that
// kept dimming a declined reservation's slot would be strictly worse than no
// picker at all — it would hide a slot the server would happily take.
func TestBookedWindows_ExcludesTerminalRides(t *testing.T) {
	for _, status := range []string{"declined", "cancelled", "completed"} {
		t.Run(status, func(t *testing.T) {
			repo, _ := setupRideRequestRepo(t)
			rec := bookAs(t, repo, windowsRider, windowsBase)

			if got := listWindows(t, repo, windowsRider, windowsBase.Add(-2*time.Hour), windowsBase.Add(2*time.Hour)); len(got) != 1 {
				t.Fatalf("precondition: expected 1 window while open, got %d", len(got))
			}

			setStatusDirect(t, rec.ID, status)

			if got := listWindows(t, repo, windowsRider, windowsBase.Add(-2*time.Hour), windowsBase.Add(2*time.Hour)); len(got) != 0 {
				t.Fatalf("a %s ride must free its window, got %+v", status, got)
			}
		})
	}
}

// TestBookedWindows_IncludesEveryCommittedStatus walks the in-progress states.
// They are in rideWindowCommittedStatuses and must therefore be in the read; a
// state present in the gate but missing here would silently under-dim.
func TestBookedWindows_IncludesEveryCommittedStatus(t *testing.T) {
	for _, status := range []string{"accepted", "arrived", "enroute"} {
		t.Run(status, func(t *testing.T) {
			repo, _ := setupRideRequestRepo(t)
			rec := bookAs(t, repo, windowsRider, windowsBase)
			setStatusDirect(t, rec.ID, status)

			got := listWindows(t, repo, windowsRider, windowsBase.Add(-2*time.Hour), windowsBase.Add(2*time.Hour))
			if len(got) != 1 {
				t.Fatalf("a %s ride must occupy its window, got %+v", status, got)
			}
			if got[0].Pending {
				t.Errorf("%s is committed, pending should be false: %+v", status, got[0])
			}
		})
	}
}

// TestBookedWindows_RangeFilteringIsStrict pins the overlap rule. The windows
// are OPEN intervals, so one that merely TOUCHES the requested range at a
// boundary blocks nothing inside it and must not be returned — returning it
// would make the picker dim a slot on the strength of a window that ends
// before the slot begins.
func TestBookedWindows_RangeFilteringIsStrict(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	bookAs(t, repo, windowsRider, windowsBase)

	winStart := windowsBase.Add(-store.RideConflictWindow)
	winEnd := windowsBase.Add(store.RideConflictWindow)

	tests := []struct {
		name     string
		from, to time.Time
		want     int
	}{
		{"range entirely before the window", windowsBase.Add(-8 * time.Hour), windowsBase.Add(-4 * time.Hour), 0},
		{"range entirely after the window", windowsBase.Add(4 * time.Hour), windowsBase.Add(8 * time.Hour), 0},
		{"range ends exactly at window start", windowsBase.Add(-8 * time.Hour), winStart, 0},
		{"range begins exactly at window end", winEnd, windowsBase.Add(8 * time.Hour), 0},
		{"range overlaps the window by a second", windowsBase.Add(-8 * time.Hour), winStart.Add(time.Second), 1},
		{"range contains the window", windowsBase.Add(-8 * time.Hour), windowsBase.Add(8 * time.Hour), 1},
		{"range strictly inside the window", windowsBase.Add(-time.Minute), windowsBase.Add(time.Minute), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := listWindows(t, repo, windowsRider, tt.from, tt.to); len(got) != tt.want {
				t.Fatalf("want %d windows, got %d: %+v", tt.want, len(got), got)
			}
		})
	}
}

// TestBookedWindows_OwnTracksTheRider pins that `own` is the RIDER, not the
// vehicle owner and not "anybody I can see". The r15 report was a rider
// colliding with their own noon reservation, and the flag exists so the picker
// can say "your ride" instead of "that car is busy".
func TestBookedWindows_OwnTracksTheRider(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	bookAs(t, repo, windowsRider, windowsBase)
	bookAs(t, repo, windowsOther, windowsBase.Add(4*time.Hour))

	from, to := windowsBase.Add(-6*time.Hour), windowsBase.Add(10*time.Hour)

	mine := listWindows(t, repo, windowsRider, from, to)
	if len(mine) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(mine))
	}
	if !mine[0].Own || mine[1].Own {
		t.Errorf("for %s: want own=[true false], got [%v %v]", windowsRider, mine[0].Own, mine[1].Own)
	}

	// The SAME two windows, read by the other rider: identical instants, the
	// flag simply flips. `own:false` says only "not yours", never who — both
	// callers see the same two intervals whatever they are told about them.
	theirs := listWindows(t, repo, windowsOther, from, to)
	if len(theirs) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(theirs))
	}
	if theirs[0].Own || !theirs[1].Own {
		t.Errorf("for %s: want own=[false true], got [%v %v]", windowsOther, theirs[0].Own, theirs[1].Own)
	}
	for i := range mine {
		if !mine[i].Start.Equal(theirs[i].Start) || !mine[i].End.Equal(theirs[i].End) {
			t.Errorf("window %d differs between callers: %+v vs %+v", i, mine[i], theirs[i])
		}
	}
}

// TestBookedWindows_ActiveInstantRideAnchorsOnNow covers the arm that has no
// stored instant: a car MID-RIDE cannot also promise a pickup twenty minutes
// out, so the gate anchors that ride's window on NOW() — and the read surface
// must report it, or a picker would leave the next hour and a half enabled for
// a car that is currently driving somebody else.
//
// It also pins the negative half: a merely `requested` instant ride occupies
// NOTHING. Nobody has promised the car yet, and dimming for it would refuse
// slots the gate would take.
func TestBookedWindows_ActiveInstantRideAnchorsOnNow(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)

	instant, err := repo.Create(context.Background(), minimalRideRequest())
	if err != nil {
		t.Fatalf("create instant ride: %v", err)
	}

	now := time.Now().UTC()
	from, to := now.Add(-4*time.Hour), now.Add(4*time.Hour)

	if got := listWindows(t, repo, windowsRider, from, to); len(got) != 0 {
		t.Fatalf("a `requested` instant ride occupies no window, got %+v", got)
	}

	mustAccept(t, repo, instant.ID)

	got := listWindows(t, repo, windowsRider, from, to)
	if len(got) != 1 {
		t.Fatalf("an accepted instant ride must occupy a window around now, got %+v", got)
	}
	// Anchored on the SERVER's clock at read time, so assert the window
	// straddles now with the right width rather than pinning an exact instant.
	if !got[0].Start.Before(now.Add(time.Minute)) || !got[0].End.After(now) {
		t.Errorf("window %+v does not straddle now (%s)", got[0], now)
	}
	if width := got[0].End.Sub(got[0].Start); width != 2*store.RideConflictWindow {
		t.Errorf("width = %s, want %s", width, 2*store.RideConflictWindow)
	}
	if got[0].Pending {
		t.Error("an active instant ride is committed by construction; pending must be false")
	}
}

// TestBookedWindows_WidthTracksTheConflictConstant is the DRIFT TRIPWIRE.
//
// It asserts the emitted width against store.RideConflictWindow by REFERENCE,
// not against a literal 90 minutes. That is the entire point: the half-width is
// a product guess, and the promise this surface makes is that changing that one
// line moves the gate and every client's picker together. If somebody re-widens
// the constant and this file still passes, the promise held. If somebody
// hard-codes the width anywhere in the emission path, this test is what catches
// it — a literal here would have made the test complicit instead.
func TestBookedWindows_WidthTracksTheConflictConstant(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	bookAs(t, repo, windowsRider, windowsBase)
	bookAs(t, repo, windowsRider, windowsBase.Add(4*time.Hour))

	got := listWindows(t, repo, windowsRider, windowsBase.Add(-6*time.Hour), windowsBase.Add(10*time.Hour))
	if len(got) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(got))
	}
	for i, w := range got {
		if width := w.End.Sub(w.Start); width != 2*store.RideConflictWindow {
			t.Errorf("window[%d] width = %s, want 2*RideConflictWindow = %s", i, width, 2*store.RideConflictWindow)
		}
	}
	// And the anchor is centred, not offset — the ride's instant sits exactly
	// in the middle of the interval it produces.
	if mid := got[0].Start.Add(store.RideConflictWindow); !mid.Equal(windowsBase) {
		t.Errorf("window[0] is not centred on the ride instant: midpoint %s, instant %s", mid, windowsBase)
	}
}

// TestBookedWindows_EmittedEdgeIsBookable proves the equivalence this whole
// surface rests on, from BOTH directions, using the gate itself as the oracle:
//
//	a booking at exactly window.End is ACCEPTED  — the edges are EXCLUSIVE
//	a booking one second inside it is REFUSED    — and they are the real edge
//
// Getting this backwards is the plausible bug. The gate's comparison is
// strictly inside the window ("exactly this far apart is allowed" — a legal
// back-to-back booking), so a picker that dimmed the endpoints would refuse
// slots the server would have taken, and a picker that dimmed one second less
// would offer a slot the server refuses. Nothing but a test that books against
// the emitted number can tell those two apart.
func TestBookedWindows_EmittedEdgeIsBookable(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	held := bookAs(t, repo, windowsRider, windowsBase)
	mustAccept(t, repo, held.ID)

	got := listWindows(t, repo, windowsRider, windowsBase.Add(-6*time.Hour), windowsBase.Add(6*time.Hour))
	if len(got) != 1 {
		t.Fatalf("expected 1 window, got %d", len(got))
	}
	edge := got[0].End

	// One second INSIDE the emitted edge: the gate refuses it, so the picker
	// was right to dim it.
	if _, err := bookReservation(t, repo, edge.Add(-time.Second)); err == nil {
		t.Fatal("a booking one second inside the emitted window must be refused")
	} else {
		_ = assertWindowConflict(t, err)
	}

	// Exactly ON the emitted edge: the gate ALLOWS it, so a picker that dimmed
	// this slot would have refused a booking the server was happy to take.
	if _, err := bookReservation(t, repo, edge); err != nil {
		t.Fatalf("a booking at exactly the emitted window edge must be allowed, got %v", err)
	}

	// Symmetrically at the start edge, on a clean fixture — the same rule from
	// the other side.
	repo2, _ := setupRideRequestRepo(t)
	held2 := bookAs(t, repo2, windowsRider, windowsBase)
	mustAccept(t, repo2, held2.ID)
	start := listWindows(t, repo2, windowsRider, windowsBase.Add(-6*time.Hour), windowsBase.Add(6*time.Hour))[0].Start
	if _, err := bookReservation(t, repo2, start.Add(time.Second)); err == nil {
		t.Fatal("a booking one second inside the emitted window start must be refused")
	}
	if _, err := bookReservation(t, repo2, start); err != nil {
		t.Fatalf("a booking at exactly the emitted window start must be allowed, got %v", err)
	}
}

// TestBookedWindows_UnknownVehicleIsEmpty pins that an unknown or free vehicle
// is an empty slice and not an error. A not-found here would confirm the
// existence of cars the caller cannot see, and "no windows" is the ordinary
// answer besides — the common case for most cars, most of the time.
func TestBookedWindows_UnknownVehicleIsEmpty(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)

	got, err := repo.ListBookedWindows(context.Background(),
		"cldoesnotexist0000000", windowsRider, windowsBase, windowsBase.Add(time.Hour))
	if err != nil {
		t.Fatalf("unknown vehicle must not error: %v", err)
	}
	if got == nil {
		t.Fatal("want an empty non-nil slice so the handler marshals `items: []`")
	}
	if len(got) != 0 {
		t.Fatalf("want no windows, got %+v", got)
	}
}

// TestBookedWindows_OrderedByStart pins the response order the picker walks in
// one pass. Fixtures are created out of order so a passing test cannot be an
// accident of insertion order.
func TestBookedWindows_OrderedByStart(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	bookAs(t, repo, windowsRider, windowsBase.Add(8*time.Hour))
	bookAs(t, repo, windowsRider, windowsBase)
	bookAs(t, repo, windowsRider, windowsBase.Add(4*time.Hour))

	got := listWindows(t, repo, windowsRider, windowsBase.Add(-6*time.Hour), windowsBase.Add(14*time.Hour))
	if len(got) != 3 {
		t.Fatalf("expected 3 windows, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i-1].Start.Before(got[i].Start) {
			t.Fatalf("windows not ascending by start: %s then %s", got[i-1].Start, got[i].Start)
		}
	}
}
