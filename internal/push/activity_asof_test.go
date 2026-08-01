package push

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// `asOf` — when the DATA was true (MYR-398, the v3 card).
//
// Every test here is written so that stamping asOf from the send clock would
// FAIL it. That is the only failure mode worth defending against: a server
// that used `now` would be correct on every healthy push and useless on
// exactly the pushes the field exists for.

// TestContentStateAsOfTracksTheReadingNotTheClock is the derivation table from
// rest-api.md §7.21.3, one subtest per row.
func TestContentStateAsOfTracksTheReadingNotTheClock(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	base := RideContext{
		Status:           "accepted",
		VehicleName:      "Blue Whale",
		Destination:      "Home",
		NavUpdatedAt:     &fresh,
		DispatchUnderway: true,
	}

	tests := []struct {
		name string
		rc   func(RideContext) RideContext
		prev ProgressAnchor
		want time.Time
	}{
		{
			// A reading first seen THIS pass. The data is as new as the push.
			name: "fresh reading newly changed",
			rc: func(rc RideContext) RideContext {
				rc.TripMilesRemaining = miles(4)
				return rc
			},
			want: fixedNow,
		},
		{
			// The car has been reporting the same distance since two minutes
			// ago — a red light, not a fault. The card is not stale so nothing
			// renders asOf, and the honest instant is still when the car last
			// said something new.
			name: "reading unchanged since an earlier pass",
			rc: func(rc RideContext) RideContext {
				rc.TripMilesRemaining = miles(4)
				return rc
			},
			prev: ProgressAnchor{
				Leg: ProgressLegPickup, Source: ProgressSourceNavDistance,
				Baseline: 10, Value: 0.6,
				Reading: 4, ReadingAt: fixedNow.Add(-2 * time.Minute),
			},
			want: fixedNow.Add(-2 * time.Minute),
		},
		{
			// THE CASE THE FIELD EXISTS FOR. The car cleared its route, the
			// fraction is held, and asOf holds with it while the push's own
			// timestamp and stale-date move on.
			name: "progress held after the nav route was cleared",
			rc: func(rc RideContext) RideContext {
				rc.TripMilesRemaining, rc.ETAMinutes = nil, nil
				return rc
			},
			prev: ProgressAnchor{
				Leg: ProgressLegPickup, Source: ProgressSourceNavDistance,
				Baseline: 10, Value: 0.62,
				Reading: 3.8, ReadingAt: fixedNow.Add(-7 * time.Minute),
			},
			want: fixedNow.Add(-7 * time.Minute),
		},
		{
			// Telemetry gone quiet: the row is stale, so the gate rejects the
			// reading outright and the fraction is held. Same answer.
			name: "progress held because the car row went stale",
			rc: func(rc RideContext) RideContext {
				old := fixedNow.Add(-10 * time.Minute)
				rc.NavUpdatedAt = &old
				rc.TripMilesRemaining = miles(3.8)
				return rc
			},
			prev: ProgressAnchor{
				Leg: ProgressLegPickup, Source: ProgressSourceNavDistance,
				Baseline: 10, Value: 0.62,
				Reading: 3.8, ReadingAt: fixedNow.Add(-9 * time.Minute),
			},
			want: fixedNow.Add(-9 * time.Minute),
		},
		{
			// `arrived` is the ride record asserting the leg is over. It rests
			// on no reading and clears the anchor, so it speaks as of now —
			// even though a reading from ten minutes ago is sitting right
			// there in the context.
			name: "arrived is an assertion, not a reading",
			rc: func(rc RideContext) RideContext {
				old := fixedNow.Add(-10 * time.Minute)
				rc.Status, rc.NavUpdatedAt = "arrived", &old
				rc.TripMilesRemaining = miles(0)
				return rc
			},
			prev: ProgressAnchor{
				Leg: ProgressLegPickup, Source: ProgressSourceNavDistance,
				Baseline: 10, Value: 0.9,
				Reading: 1, ReadingAt: fixedNow.Add(-10 * time.Minute),
			},
			want: fixedNow,
		},
		{
			// A ride that never happened has no reading behind it.
			name: "cancelled speaks as of now",
			rc: func(rc RideContext) RideContext {
				rc.Status = "cancelled"
				return rc
			},
			want: fixedNow,
		},
		{
			// Dispatch: no car assigned, nothing to be old.
			name: "requested speaks as of now",
			rc: func(rc RideContext) RideContext {
				rc.Status = statusRequested
				return rc
			},
			want: fixedNow,
		},
		{
			// A dormant reservation stores no anchor at all, by MYR-398's
			// dormancy gate, so there is nothing for asOf to lag behind.
			name: "dormant reservation speaks as of now",
			rc: func(rc RideContext) RideContext {
				rc.DispatchUnderway = false
				rc.TripMilesRemaining = miles(4)
				return rc
			},
			want: fixedNow,
		},
		{
			// Never heard from the car on this leg: no reading, no anchor.
			name: "no telemetry at all speaks as of now",
			rc: func(rc RideContext) RideContext {
				rc.NavUpdatedAt = nil
				return rc
			},
			want: fixedNow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, _ := contentState(tc.rc(base), tc.prev, fixedNow)

			if state.AsOf == nil {
				t.Fatal("asOf is absent; the server always knows when it computed a state")
			}
			if got, want := *state.AsOf, tc.want.Unix(); got != want {
				t.Errorf("asOf = %d, want %d (delta from now: got %s, want %s)",
					got, want,
					time.Duration(got-fixedNow.Unix())*time.Second,
					tc.want.Sub(fixedNow))
			}
			// The invariant that holds in every row: asOf can lag the send
			// instant but can never lead it.
			if *state.AsOf > fixedNow.Unix() {
				t.Errorf("asOf = %d is in the FUTURE relative to the send instant %d",
					*state.AsOf, fixedNow.Unix())
			}
		})
	}
}

// TestHeldProgressKeepsAsOfAcrossTicks is the same promise from the outside:
// two ticker passes over a car that has stopped reporting, where the second
// push must carry the FIRST push's asOf.
//
// A unit test on contentState cannot see this — it is the round trip through
// the persisted anchor that decides it, which is the same reason
// TestActivityTickerAdvancesTheTrack exists beside it.
func TestHeldProgressKeepsAsOfAcrossTicks(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	n, sender, store := newTestActivityNotifier(t, nil)
	store.legs = []ActivityLeg{{
		Activity: Activity{
			RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken,
			// Seeded past the ladder so the leg flip does not attach an alert
			// and change what this test is about.
			AlertedPhase: AlertPhaseOnTrip,
		},
		RideContext: RideContext{
			Status:      statusEnroute,
			VehicleName: "Blue Whale",
			Destination: "Home",
			// The car's carried minutes are frozen at 12 from the moment it
			// went quiet — which is exactly how a stale ETA keeps producing a
			// fresh-looking arrival instant, twelve minutes out, pass after
			// pass. `eta` is ungated by design (§7.21.3).
			ETAMinutes:         intPtr(12),
			TripMilesRemaining: miles(10),
			NavUpdatedAt:       &fresh,
		},
	}}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	ticker.RunPass(t.Context())

	// The car goes quiet: same reading, and the pass happens well past the
	// freshness horizon.
	later := fixedNow.Add(20 * time.Minute)
	n.now = func() time.Time { return later }
	ticker.RunPass(t.Context())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want 2", len(sent))
	}

	first, second := sent[0].ContentState, sent[1].ContentState
	if first.AsOf == nil || second.AsOf == nil {
		t.Fatalf("asOf absent: first=%s second=%s", fmtPtr(first.AsOf), fmtPtr(second.AsOf))
	}
	if *second.AsOf != *first.AsOf {
		t.Errorf("second push asOf = %d, want %d — a held fraction must not re-stamp asOf",
			*second.AsOf, *first.AsOf)
	}

	// And the three things that DID move, which is what makes the held asOf
	// load-bearing rather than decorative. If any of these had also frozen,
	// ActivityKit or the card itself would already have said the card was out
	// of date and the field would be redundant.
	if sent[1].Timestamp.Equal(sent[0].Timestamp) {
		t.Error("aps.timestamp did not move between passes; it must always be the send instant")
	}
	if !sent[1].StaleDate().After(sent[0].StaleDate()) {
		t.Error("stale-date did not move; the push re-arms it, which is why asOf is needed")
	}
	if first.ETA == nil || second.ETA == nil || *second.ETA <= *first.ETA {
		t.Errorf("eta did not move: first=%s second=%s — it is rebuilt from each push's own now, "+
			"so the card looks fresh and only asOf says otherwise",
			fmtPtr(first.ETA), fmtPtr(second.ETA))
	}
	// The fraction is the held one, unchanged.
	if second.Progress == nil || *second.Progress != *first.Progress {
		t.Errorf("progress = %s, want the held %s", fmtPtr(second.Progress), fmtPtr(first.Progress))
	}
}

// TestLifecyclePushStampsAsOfAtTheTransition covers the other half of the rule
// from the outside: a status change is the ride record speaking now, so its
// push carries the send instant even when the nav data behind it is old.
func TestLifecyclePushStampsAsOfAtTheTransition(t *testing.T) {
	stale := fixedNow.Add(-30 * time.Minute)
	n, sender, store := newTestActivityNotifier(t, nil)
	rc := store.context[activityRideID]
	rc.Status = "arrived"
	rc.NavUpdatedAt = &stale
	rc.TripMilesRemaining = miles(0)
	store.context[activityRideID] = rc

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID, RiderID: testRiderID, Status: "arrived",
	}))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(sent))
	}
	if got := sent[0].ContentState.AsOf; got == nil || *got != fixedNow.Unix() {
		t.Errorf("asOf = %s, want %d — `arrived` is the owner confirming the car is at the "+
			"kerb, which is true as of now however old the car's navigation is",
			fmtPtr(got), fixedNow.Unix())
	}
}

// TestContractGoneQuietExampleIsInternallyConsistent decodes §7.21.4's
// gone-quiet payload and asserts the relationships the prose claims about it.
//
// Decoding the DOCUMENT rather than a hand-written twin is the point, and here
// it is worth more than usual: the example's whole job is to show four numbers
// that disagree in a specific way, and a document that quietly made them agree
// would teach a client author to ignore the field.
func TestContractGoneQuietExampleIsInternallyConsistent(t *testing.T) {
	var state ActivityContentState
	dec := json.NewDecoder(strings.NewReader(contractExampleGoneQuiet))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		t.Fatalf("the contract's printed gone-quiet content-state does not decode: %v", err)
	}

	if state.AsOf == nil {
		t.Fatal("the printed example carries no asOf")
	}
	lag := time.Duration(contractExampleGoneQuietTimestamp-*state.AsOf) * time.Second
	if lag <= StaleAfter {
		t.Errorf("printed asOf lags the push by %s, which is inside the %s horizon — the example "+
			"is meant to depict a card that has stopped learning", lag, StaleAfter)
	}
	// The eta is AHEAD of the push, which is exactly the misleading part the
	// example exists to show: it is rebuilt from the push's own now.
	if state.ETA == nil || *state.ETA <= contractExampleGoneQuietTimestamp {
		t.Errorf("printed eta = %s, want an instant after the push's timestamp %d — the point "+
			"of the example is that the arrival time still looks current",
			fmtPtr(state.ETA), contractExampleGoneQuietTimestamp)
	}
	if state.Progress == nil {
		t.Error("printed progress is absent; the example depicts a HELD fraction, not a withdrawn one")
	}
}
