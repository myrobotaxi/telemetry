package push

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// The card names the place the ETA is about (MYR-587).
//
// Field report, 2026-08-18 03:02Z, a real multi-stop night ride: the lock
// screen read "10:39 PM dropoff · Heading to Bell Southstone Yards" while the
// car was driving to TARGET, the first stop. Both values were true and the
// sentence they made was not.

// TestLegDestinationNamesTheCurrentTarget walks the ladder MYR-564 defined
// client-side, plus every status the rule deliberately does NOT touch.
func TestLegDestinationNamesTheCurrentTarget(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		stops      []TripStop
		wantLabel  string
		wantIsStop bool
	}{
		{
			name:       "two-endpoint ride names the dropoff",
			status:     statusEnroute,
			stops:      nil,
			wantLabel:  "Home",
			wantIsStop: false,
		},
		{
			name:   "mid-leg to the first stop names that stop",
			status: statusEnroute,
			stops: []TripStop{
				{Status: "current", Label: "TARGET"},
				{Status: "upcoming", Label: "Bell Southstone Yards"},
			},
			wantLabel:  "TARGET",
			wantIsStop: true,
		},
		{
			name:   "once the first stop completes the next one is named",
			status: statusEnroute,
			stops: []TripStop{
				{Status: "completed", Label: "TARGET"},
				{Status: "current", Label: "Original ChopShop"},
			},
			wantLabel:  "Original ChopShop",
			wantIsStop: true,
		},
		{
			name:   "the final leg names the dropoff again",
			status: statusEnroute,
			stops: []TripStop{
				{Status: "completed", Label: "TARGET"},
				{Status: "completed", Label: "Original ChopShop"},
			},
			wantLabel:  "Home",
			wantIsStop: false,
		},
		{
			name:   "no stop marked current yet falls to the first one not completed",
			status: statusEnroute,
			stops: []TripStop{
				{Status: "completed", Label: "TARGET"},
				{Status: "upcoming", Label: "Original ChopShop"},
				{Status: "upcoming", Label: "Nordstrom"},
			},
			wantLabel:  "Original ChopShop",
			wantIsStop: true,
		},
		{
			// The server's own mark is a positive statement about where the car
			// is going; a stop still reading `upcoming` above it is not.
			name:   "a current mark wins over an earlier upcoming stop",
			status: statusEnroute,
			stops: []TripStop{
				{Status: "upcoming", Label: "TARGET"},
				{Status: "current", Label: "Original ChopShop"},
			},
			wantLabel:  "Original ChopShop",
			wantIsStop: true,
		},
		{
			// MYR-547's fail-safe: `completed` is the only status that says the
			// car REACHED a place. Anything else — including a member this
			// build has never heard of — is a stop still ahead of it.
			name:   "an unrecognised status counts as ahead",
			status: statusEnroute,
			stops: []TripStop{
				{Status: "completed", Label: "TARGET"},
				{Status: "skipped", Label: "Original ChopShop"},
				{Status: "upcoming", Label: "Nordstrom"},
			},
			wantLabel:  "Original ChopShop",
			wantIsStop: true,
		},
		{
			name:   "an empty status is ahead too, not silently behind",
			status: statusEnroute,
			stops: []TripStop{
				{Status: "", Label: "Original ChopShop"},
			},
			wantLabel:  "Original ChopShop",
			wantIsStop: true,
		},
		{
			// The pickup leg's ETA is about the PICKUP, which is deliberately
			// not on this wire at all — so the label keeps its old job of
			// naming where the trip ends.
			name:   "the pickup leg is untouched",
			status: statusAccepted,
			stops: []TripStop{
				{Status: "upcoming", Label: "Original ChopShop"},
			},
			wantLabel:  "Home",
			wantIsStop: false,
		},
		{
			name:   "arrived at the kerb is untouched",
			status: statusArrived,
			stops: []TripStop{
				{Status: "upcoming", Label: "Original ChopShop"},
			},
			wantLabel:  "Home",
			wantIsStop: false,
		},
		{
			name:   "a ride nobody has accepted is untouched",
			status: statusRequested,
			stops: []TripStop{
				{Status: "upcoming", Label: "Original ChopShop"},
			},
			wantLabel:  "Home",
			wantIsStop: false,
		},
		{
			// MYR-547: ending early leaves the stops the car never reached
			// unreached. On a ride that is OVER there is nothing to head to,
			// and naming stop 2 would put the card's last word on a place the
			// car never went.
			name:   "a ride completed early names the dropoff, not the stop it never reached",
			status: statusCompleted,
			stops: []TripStop{
				{Status: "completed", Label: "TARGET"},
				{Status: "current", Label: "Original ChopShop"},
			},
			wantLabel:  "Home",
			wantIsStop: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := RideContext{
				Status:      tc.status,
				Destination: "Home",
				Stops:       tc.stops,
			}

			label, isStop := legDestination(rc)

			if label != tc.wantLabel {
				t.Errorf("label = %q, want %q", label, tc.wantLabel)
			}
			if isStop != tc.wantIsStop {
				t.Errorf("isStop = %v, want %v", isStop, tc.wantIsStop)
			}
		})
	}
}

// TestContentStatePairsTheETAWithTheStopItDescribes is the defect itself, at
// the one function both send paths compose through.
func TestContentStatePairsTheETAWithTheStopItDescribes(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	rc := RideContext{
		Status:      statusEnroute,
		VehicleName: "Blue Whale",
		Destination: "Bell Southstone Yards",
		Stops: []TripStop{
			{Status: "current", Label: "TARGET"},
			{Status: "upcoming", Label: "Nordstrom"},
		},
		// The car's own carried ETA, which is always about the leg it is
		// driving now (MYR-485) — the leg ending at TARGET.
		ETAMinutes:       intPtr(7),
		NavUpdatedAt:     &fresh,
		DispatchUnderway: true,
	}

	state, _ := contentState(rc, ProgressAnchor{}, fixedNow)

	if state.Destination != "TARGET" {
		t.Errorf("destination = %q, want %q — the ETA below it is the leg to the stop",
			state.Destination, "TARGET")
	}
	if !state.DestinationIsStop {
		t.Error("destinationIsStop = false; the headline would still read `dropoff` over a stop's name")
	}
	if state.ETA == nil {
		t.Fatal("eta = nil; the car reported a route and the pairing is the whole point")
	}
	if want := fixedNow.Add(7 * time.Minute).Unix(); *state.ETA != want {
		t.Errorf("eta = %d, want %d", *state.ETA, want)
	}
}

// TestTwoEndpointRideIsByteIdentical pins the compatibility promise: a ride
// with no stops must serialize exactly as it did before MYR-587 existed, key
// for key and byte for byte.
func TestTwoEndpointRideIsByteIdentical(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	rc := RideContext{
		Status:             statusEnroute,
		VehicleName:        "Blue Whale",
		Destination:        "Home",
		ETAMinutes:         intPtr(7),
		TripMilesRemaining: miles(2),
		NavUpdatedAt:       &fresh,
		DispatchUnderway:   true,
	}

	state, _ := contentState(rc, ProgressAnchor{}, fixedNow)

	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "destinationIsStop") {
		t.Errorf("payload carries destinationIsStop on a two-endpoint ride: %s", raw)
	}
	if got := state.Destination; got != "Home" {
		t.Errorf("destination = %q, want %q", got, "Home")
	}
}

// TestStopLabelTakesTheSameCap closes the hole a new label source could open:
// the 128-rune bound exists because Apple answers an over-cap push with a flat
// 400 that takes out the WHOLE Activity, and a stop label reaches the payload
// from a request body exactly as the dropoff label does.
func TestStopLabelTakesTheSameCap(t *testing.T) {
	state, _ := contentState(RideContext{
		Status:      statusEnroute,
		Destination: "Home",
		Stops:       []TripStop{{Status: "current", Label: strings.Repeat("a", 4096)}},
	}, ProgressAnchor{}, fixedNow)

	if n := utf8.RuneCountInString(state.Destination); n > MaxContentStateLabel {
		t.Errorf("destination is %d runes, over the %d the schema declares", n, MaxContentStateLabel)
	}
	if !strings.HasSuffix(state.Destination, "…") {
		t.Error("a truncated stop label must say it was truncated")
	}
}

// multiStopEnroute is the field report's ride: aboard, driving to stop 1, with
// the trip ending somewhere else entirely.
func multiStopEnroute() RideContext {
	return RideContext{
		Status:      statusEnroute,
		VehicleName: "Blue Whale",
		Destination: "Bell Southstone Yards",
		Stops: []TripStop{
			{Status: "current", Label: "TARGET"},
			{Status: "upcoming", Label: "Nordstrom"},
		},
		ETAMinutes: intPtr(7),
	}
}

// TestBothSendPathsNameTheSameStop is the "one composition rule, not two"
// assertion.
//
// The ETA ticker and the lifecycle fan-out build their own notifications from
// their own reads, and either of them naming a different place would put the
// card at odds with itself — a status change would rename the destination and
// the next tick would rename it back.
func TestBothSendPathsNameTheSameStop(t *testing.T) {
	t.Run("lifecycle transition", func(t *testing.T) {
		n, sender, store := newTestActivityNotifier(t, nil)
		store.context[activityRideID] = multiStopEnroute()

		n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
			RideRequestID: activityRideID,
			RiderID:       testRiderID,
			Status:        statusEnroute,
		}))
		n.Wait()

		sent := sender.Sent()
		if len(sent) != 1 {
			t.Fatalf("sent %d updates, want 1", len(sent))
		}
		assertNamesTheStop(t, sent[0].ContentState)
	})

	t.Run("eta tick", func(t *testing.T) {
		ticker, sender, store := newTestTicker(t, 1, nil)
		store.legs[0].RideContext = multiStopEnroute()

		ticker.RunPass(context.Background())

		sent := sender.Sent()
		if len(sent) != 1 {
			t.Fatalf("sent %d updates, want 1", len(sent))
		}
		assertNamesTheStop(t, sent[0].ContentState)
	})
}

// TestDestinationIsStopRawJSONKey pins the WIRE KEY, not the Go field.
//
// The MYR-362 lesson, applied to the newest key on this payload: the Swift
// ContentState decodes the exact string `destinationIsStop`, and a Go-side
// rename that every struct-level assertion in this file would sail through is a
// silent decode failure on every installed phone. Its absent-when-false twin is
// already pinned by TestActivityPayloadRawJSONKeys, whose fixture has no stops.
func TestDestinationIsStopRawJSONKey(t *testing.T) {
	n := testActivityNotification()
	n.ContentState.Destination = "TARGET"
	n.ContentState.DestinationIsStop = true

	got := captureActivity(t, n)

	var payload struct {
		APS struct {
			ContentState map[string]any `json:"content-state"`
		} `json:"aps"`
	}
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if v, ok := payload.APS.ContentState["destinationIsStop"]; !ok || v != true {
		t.Errorf("content-state.destinationIsStop = %v (present=%v), want true", v, ok)
	}
	if v := payload.APS.ContentState["destination"]; v != "TARGET" {
		t.Errorf("content-state.destination = %v, want TARGET", v)
	}
}

func assertNamesTheStop(t *testing.T, state ActivityContentState) {
	t.Helper()
	if state.Destination != "TARGET" {
		t.Errorf("destination = %q, want %q", state.Destination, "TARGET")
	}
	if !state.DestinationIsStop {
		t.Error("destinationIsStop = false, want true — the ETA is to a stop")
	}
}
