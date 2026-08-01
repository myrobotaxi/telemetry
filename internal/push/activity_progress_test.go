package push

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// Leg-progress derivation (MYR-398).
//
// Every case here is a claim the contract makes in rest-api.md §7.21.3. The
// ones that assert nil are the important half: an absent progress renders a
// trackless card, a wrong one renders a lie, and the whole design of this
// function is a list of situations in which it declines to answer.

func miles(v float64) *float64 { return &v }

// fmtPtr renders an optional value for a FAILURE MESSAGE.
//
// It exists because `%v` on a *float64 prints a heap address, which tells the
// reader of a CI-only failure nothing at all — and a progress assertion that
// cannot say which number it got is the one diagnostic this package most needs.
func fmtPtr[T any](p *T) string {
	if p == nil {
		return "omitted"
	}
	return fmt.Sprintf("%v", *p)
}

// legRC builds a ride context for a leg that is genuinely UNDERWAY, with a nav
// reading that is fresh as of fixedNow.
//
// DispatchUnderway is true here because that is the ordinary case every one of
// these assertions is about — a car actually driving. The dormant reservation
// it excludes has its own cases below; see legUnderway for why the distinction
// is not cosmetic.
func legRC(status string, dist *float64, eta *int) RideContext {
	fresh := fixedNow.Add(-30 * time.Second)
	return RideContext{
		Status: status, ETAMinutes: eta, TripMilesRemaining: dist,
		NavUpdatedAt: &fresh, DispatchUnderway: true,
	}
}

// dormantRC is a reservation accepted but not yet dispatched — status
// `accepted`, exactly like a car on its way, with the owner's own live nav
// reading attached because that is what the ticker actually reads.
func dormantRC(dist *float64, eta *int) RideContext {
	rc := legRC(statusAccepted, dist, eta)
	rc.DispatchUnderway = false
	return rc
}

func TestComputeProgress(t *testing.T) {
	stale := fixedNow.Add(-ProgressFreshFor - time.Second)

	tests := []struct {
		name     string
		rc       RideContext
		prev     ProgressAnchor
		want     *float64
		wantLeg  ProgressLeg
		wantSrc  ProgressSource
		wantBase float64
		wantVal  float64
	}{
		{
			name:     "first observation of leg 1 anchors at zero",
			rc:       legRC(statusAccepted, miles(10), intPtr(20)),
			want:     miles(0),
			wantLeg:  ProgressLegPickup,
			wantSrc:  ProgressSourceNavDistance,
			wantBase: 10,
		},
		{
			name:     "nav distance ratio is preferred over the eta",
			rc:       legRC(statusAccepted, miles(2.5), intPtr(19)),
			prev:     ProgressAnchor{Leg: ProgressLegPickup, Source: ProgressSourceNavDistance, Baseline: 10},
			want:     miles(0.75),
			wantLeg:  ProgressLegPickup,
			wantSrc:  ProgressSourceNavDistance,
			wantBase: 10,
			wantVal:  0.75,
		},
		{
			name:     "eta ratio is the fallback when the car reports no distance",
			rc:       legRC(statusEnroute, nil, intPtr(3)),
			prev:     ProgressAnchor{Leg: ProgressLegDropoff, Source: ProgressSourceETA, Baseline: 12},
			want:     miles(0.75),
			wantLeg:  ProgressLegDropoff,
			wantSrc:  ProgressSourceETA,
			wantBase: 12,
			wantVal:  0.75,
		},
		{
			name: "no nav data at all and no history sends nothing",
			rc:   RideContext{Status: statusAccepted, DispatchUnderway: true},
			want: nil,
		},
		{
			name: "a stale reading with no history sends nothing",
			rc:   RideContext{Status: statusAccepted, TripMilesRemaining: miles(4), NavUpdatedAt: &stale, DispatchUnderway: true},
			want: nil,
		},
		{
			name:     "a stale reading HOLDS the fraction already delivered",
			rc:       RideContext{Status: statusAccepted, TripMilesRemaining: miles(1), NavUpdatedAt: &stale, DispatchUnderway: true},
			prev:     ProgressAnchor{Leg: ProgressLegPickup, Source: ProgressSourceNavDistance, Baseline: 10, Value: 0.4},
			want:     miles(0.4),
			wantLeg:  ProgressLegPickup,
			wantSrc:  ProgressSourceNavDistance,
			wantBase: 10,
			wantVal:  0.4,
		},
		{
			name: "arrived is a full track on the ride record's authority, and clears the anchor",
			rc:   legRC(statusArrived, miles(0.2), intPtr(1)),
			prev: ProgressAnchor{Leg: ProgressLegPickup, Source: ProgressSourceNavDistance, Baseline: 10, Value: 0.9},
			want: miles(1),
		},
		{
			name: "completed is a full track too",
			rc:   legRC(statusCompleted, nil, nil),
			prev: ProgressAnchor{Leg: ProgressLegDropoff, Source: ProgressSourceETA, Baseline: 12, Value: 0.8},
			want: miles(1),
		},
		{
			name: "a cancelled ride has no fraction of itself completed",
			rc:   legRC("cancelled", miles(4), intPtr(9)),
			prev: ProgressAnchor{Leg: ProgressLegDropoff, Source: ProgressSourceETA, Baseline: 12, Value: 0.8},
			want: nil,
		},
		{
			name: "a reservation that expired has no track either",
			rc:   legRC("reservation_expired", nil, nil),
			want: nil,
		},
		{
			name:     "leg two does not inherit leg one's baseline",
			rc:       legRC(statusEnroute, miles(8), intPtr(15)),
			prev:     ProgressAnchor{Leg: ProgressLegPickup, Source: ProgressSourceNavDistance, Baseline: 10, Value: 0.9},
			want:     miles(0),
			wantLeg:  ProgressLegDropoff,
			wantSrc:  ProgressSourceNavDistance,
			wantBase: 8,
		},
		{
			name:     "traffic moving the eta backwards does not move the track backwards",
			rc:       legRC(statusEnroute, nil, intPtr(9)),
			prev:     ProgressAnchor{Leg: ProgressLegDropoff, Source: ProgressSourceETA, Baseline: 12, Value: 0.5},
			want:     miles(0.5),
			wantLeg:  ProgressLegDropoff,
			wantSrc:  ProgressSourceETA,
			wantBase: 12,
			wantVal:  0.5,
		},
		{
			name: "a reroute past the baseline re-anchors without moving the arrow",
			rc:   legRC(statusEnroute, miles(15), intPtr(30)),
			prev: ProgressAnchor{Leg: ProgressLegDropoff, Source: ProgressSourceNavDistance, Baseline: 12, Value: 0.5},
			want: miles(0.5),
			// 15 miles now MEANS half way, so the leg is 30 miles long.
			wantLeg:  ProgressLegDropoff,
			wantSrc:  ProgressSourceNavDistance,
			wantBase: 30,
			wantVal:  0.5,
		},
		{
			name:     "switching from distance to minutes re-anchors rather than dividing miles by minutes",
			rc:       legRC(statusEnroute, nil, intPtr(6)),
			prev:     ProgressAnchor{Leg: ProgressLegDropoff, Source: ProgressSourceNavDistance, Baseline: 12, Value: 0.5},
			want:     miles(0.5),
			wantLeg:  ProgressLegDropoff,
			wantSrc:  ProgressSourceETA,
			wantBase: 12,
			wantVal:  0.5,
		},
		{
			name:     "an in-flight leg never reports a full track",
			rc:       legRC(statusAccepted, miles(0.001), intPtr(0)),
			prev:     ProgressAnchor{Leg: ProgressLegPickup, Source: ProgressSourceNavDistance, Baseline: 10},
			want:     miles(MaxInFlightProgress),
			wantLeg:  ProgressLegPickup,
			wantSrc:  ProgressSourceNavDistance,
			wantBase: 10,
			wantVal:  MaxInFlightProgress,
		},
		{
			name:    "zero distance remaining is all-but-arrived, not arrived",
			rc:      legRC(statusAccepted, miles(0), intPtr(0)),
			want:    miles(MaxInFlightProgress),
			wantLeg: ProgressLegPickup,
			wantSrc: ProgressSourceNavDistance,
			wantVal: MaxInFlightProgress,
		},
		{
			name:     "a NaN distance is not a distance",
			rc:       legRC(statusAccepted, miles(math.NaN()), intPtr(5)),
			prev:     ProgressAnchor{Leg: ProgressLegPickup, Source: ProgressSourceETA, Baseline: 20},
			want:     miles(0.75),
			wantLeg:  ProgressLegPickup,
			wantSrc:  ProgressSourceETA,
			wantBase: 20,
			wantVal:  0.75,
		},
		{
			name:     "the emitted fraction is rounded to three decimals",
			rc:       legRC(statusAccepted, miles(1), intPtr(2)),
			prev:     ProgressAnchor{Leg: ProgressLegPickup, Source: ProgressSourceNavDistance, Baseline: 3},
			want:     miles(0.667),
			wantLeg:  ProgressLegPickup,
			wantSrc:  ProgressSourceNavDistance,
			wantBase: 3,
			wantVal:  0.667,
		},
		{
			// The dormancy gate. The reading is real and fresh — it is the
			// owner's own car, mid-errand, the day before the reservation.
			name: "a dormant reservation has no track, whatever the owner's car is doing",
			rc:   dormantRC(miles(5), intPtr(11)),
			want: nil,
		},
		{
			// ... and it must not merely omit the key: an anchor persisted here
			// would become the real leg's floor, since the status never changes.
			name: "a dormant reservation stores no anchor to poison the real leg with",
			rc:   dormantRC(miles(0), intPtr(0)),
			prev: ProgressAnchor{Leg: ProgressLegPickup, Source: ProgressSourceNavDistance, Baseline: 5, Value: 0.6},
			want: nil,
		},
		{
			// §7.21.3's traffic row, on the source that actually needs it. 20
			// minutes against a 12-minute baseline is TRAFFIC, not a reroute:
			// the baseline must not move, or the leg is silently redefined as
			// 40 minutes long and every later reading renders inflated.
			name:     "traffic past the eta baseline holds the fraction and leaves the baseline alone",
			rc:       legRC(statusEnroute, nil, intPtr(20)),
			prev:     ProgressAnchor{Leg: ProgressLegDropoff, Source: ProgressSourceETA, Baseline: 12, Value: 0.5},
			want:     miles(0.5),
			wantLeg:  ProgressLegDropoff,
			wantSrc:  ProgressSourceETA,
			wantBase: 12,
			wantVal:  0.5,
		},
		{
			// The row §7.21.3 now prints as its own: a car still streaming, with
			// a fraction already delivered, that CLEARS its nav route. The
			// readings go absent while the row stays fresh, and the server holds
			// rather than withdrawing the track.
			name:     "a nav route cleared mid-leg holds the last delivered fraction",
			rc:       legRC(statusEnroute, nil, nil),
			prev:     ProgressAnchor{Leg: ProgressLegDropoff, Source: ProgressSourceNavDistance, Baseline: 20, Value: 0.35},
			want:     miles(0.35),
			wantLeg:  ProgressLegDropoff,
			wantSrc:  ProgressSourceNavDistance,
			wantBase: 20,
			wantVal:  0.35,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, anchor := computeProgress(tt.rc, tt.prev, fixedNow)

			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("progress = %v, want omitted", *got)
			case tt.want != nil && got == nil:
				t.Fatalf("progress omitted, want %v", *tt.want)
			case tt.want != nil && math.Abs(*got-*tt.want) > 1e-9:
				t.Errorf("progress = %v, want %v", *got, *tt.want)
			}

			want := ProgressAnchor{Leg: tt.wantLeg, Source: tt.wantSrc, Baseline: tt.wantBase, Value: tt.wantVal}
			if anchor.Leg != want.Leg || anchor.Source != want.Source ||
				math.Abs(anchor.Baseline-want.Baseline) > 1e-9 || math.Abs(anchor.Value-want.Value) > 1e-9 {
				t.Errorf("anchor = %+v, want %+v", anchor, want)
			}
		})
	}
}

// TestProgressIsMonotoneAcrossALeg walks a whole leg the way the ticker does —
// feeding each push's anchor into the next — and asserts the sequence the
// CLIENT sees never decreases. That framing is the point: monotonicity is a
// property of the delivered sequence, not of the car, and a per-call assertion
// would not catch a re-anchoring that quietly lets the next value dip.
func TestProgressIsMonotoneAcrossALeg(t *testing.T) {
	// Miles remaining, including a reroute (12 → 15) and two traffic wobbles.
	readings := []float64{20, 18, 19, 14, 12, 15, 13, 13.5, 9, 4, 1, 0}

	var anchor ProgressAnchor
	last := -1.0
	for i, r := range readings {
		got, next := computeProgress(legRC(statusEnroute, miles(r), nil), anchor, fixedNow)
		if got == nil {
			t.Fatalf("reading %d (%v miles): progress omitted with a live anchor", i, r)
		}
		if *got < last {
			t.Errorf("reading %d (%v miles): progress %v < previous %v — the track ran backwards", i, r, *got, last)
		}
		if *got > MaxInFlightProgress {
			t.Errorf("reading %d (%v miles): progress %v exceeds the in-flight cap", i, r, *got)
		}
		last, anchor = *got, next
	}
	if last <= 0.9 {
		t.Errorf("a leg driven to zero miles ended at %v; the track should be nearly full", last)
	}
}

// TestProgressLegFlipResetsTheTrack walks the leg boundary the way a real ride
// does — accepted, the owner confirms pickup, the rider starts — and asserts
// leg two opens at the start of the track rather than where leg one ended.
func TestProgressLegFlipResetsTheTrack(t *testing.T) {
	var anchor ProgressAnchor

	p, anchor := computeProgress(legRC(statusAccepted, miles(10), nil), anchor, fixedNow)
	if p == nil || *p != 0 {
		t.Fatalf("leg 1 opening progress = %s, want 0", fmtPtr(p))
	}
	p, anchor = computeProgress(legRC(statusAccepted, miles(2), nil), anchor, fixedNow)
	if p == nil || *p != 0.8 {
		t.Fatalf("leg 1 progress = %s, want 0.8", fmtPtr(p))
	}

	p, anchor = computeProgress(legRC(statusArrived, miles(0), nil), anchor, fixedNow)
	if p == nil || *p != 1 {
		t.Fatalf("arrived progress = %s, want 1", fmtPtr(p))
	}
	if anchor != (ProgressAnchor{}) {
		t.Errorf("arrived left anchor %+v; leg 2 must start from nothing", anchor)
	}

	p, anchor = computeProgress(legRC(statusEnroute, miles(30), nil), anchor, fixedNow)
	if p == nil || *p != 0 {
		t.Fatalf("leg 2 opening progress = %s, want 0", fmtPtr(p))
	}
	if anchor.Leg != ProgressLegDropoff || anchor.Baseline != 30 {
		t.Errorf("leg 2 anchor = %+v, want the dropoff leg baselined at 30", anchor)
	}
}

// TestProgressFreshnessBoundary pins the gate exactly at ProgressFreshFor. One
// second either side is the difference between "the arrow moves" and "the arrow
// holds", and the contract states the horizon in as many words.
func TestProgressFreshnessBoundary(t *testing.T) {
	prev := ProgressAnchor{Leg: ProgressLegPickup, Source: ProgressSourceNavDistance, Baseline: 10, Value: 0.2}

	for _, tc := range []struct {
		name string
		age  time.Duration
		want float64
	}{
		{"just inside the horizon advances", ProgressFreshFor - time.Second, 0.5},
		{"exactly at the horizon still advances", ProgressFreshFor, 0.5},
		{"one second past it holds", ProgressFreshFor + time.Second, 0.2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := fixedNow.Add(-tc.age)
			rc := RideContext{Status: statusAccepted, TripMilesRemaining: miles(5), NavUpdatedAt: &at, DispatchUnderway: true}
			got, _ := computeProgress(rc, prev, fixedNow)
			if got == nil || *got != tc.want {
				t.Fatalf("progress = %s, want %v", fmtPtr(got), tc.want)
			}
		})
	}
}
