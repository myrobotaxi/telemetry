package push

import (
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

// legRC builds a ride context with a nav reading that is fresh as of fixedNow.
func legRC(status string, dist *float64, eta *int) RideContext {
	fresh := fixedNow.Add(-30 * time.Second)
	return RideContext{Status: status, ETAMinutes: eta, TripMilesRemaining: dist, NavUpdatedAt: &fresh}
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
			rc:   RideContext{Status: statusAccepted},
			want: nil,
		},
		{
			name: "a stale reading with no history sends nothing",
			rc:   RideContext{Status: statusAccepted, TripMilesRemaining: miles(4), NavUpdatedAt: &stale},
			want: nil,
		},
		{
			name:     "a stale reading HOLDS the fraction already delivered",
			rc:       RideContext{Status: statusAccepted, TripMilesRemaining: miles(1), NavUpdatedAt: &stale},
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
		t.Fatalf("leg 1 opening progress = %v, want 0", p)
	}
	p, anchor = computeProgress(legRC(statusAccepted, miles(2), nil), anchor, fixedNow)
	if p == nil || *p != 0.8 {
		t.Fatalf("leg 1 progress = %v, want 0.8", p)
	}

	p, anchor = computeProgress(legRC(statusArrived, miles(0), nil), anchor, fixedNow)
	if p == nil || *p != 1 {
		t.Fatalf("arrived progress = %v, want 1", p)
	}
	if anchor != (ProgressAnchor{}) {
		t.Errorf("arrived left anchor %+v; leg 2 must start from nothing", anchor)
	}

	p, anchor = computeProgress(legRC(statusEnroute, miles(30), nil), anchor, fixedNow)
	if p == nil || *p != 0 {
		t.Fatalf("leg 2 opening progress = %v, want 0", p)
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
			rc := RideContext{Status: statusAccepted, TripMilesRemaining: miles(5), NavUpdatedAt: &at}
			got, _ := computeProgress(rc, prev, fixedNow)
			if got == nil || *got != tc.want {
				t.Fatalf("progress = %v, want %v", got, tc.want)
			}
		})
	}
}
