package push

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// errTestSendFailed stands in for any transport refusal that is not a
// permanent token rejection.
var errTestSendFailed = errors.New("apns refused the push")

// The progress field on the wire (MYR-398).
//
// Same discipline as TestActivityPayloadRawJSONKeys beside it, and the same
// MYR-362 lesson: the Swift ContentState decodes the literal string "progress",
// so nothing in this file reads a value through a Go field name.

// contractExampleWithProgress is the routine ETA update printed in
// rest-api.md §7.21.4, copied byte for byte.
//
// Decoding the DOCUMENT rather than a hand-written twin of it is the point: if
// the printed example and the struct ever disagree, one of them is lying to a
// client SDK author, and this test is what notices.
const contractExampleWithProgress = `{
    "v": 1,
    "status": "enroute",
    "eta": 1785535200,
    "vehicleName": "Blue Whale",
    "destination": "Home",
    "progress": 0.62,
    "asOf": 1785534930
  }`

// contractExampleGoneQuiet is §7.21.4's "car that has gone quiet mid-leg"
// example, copied byte for byte.
//
// It is here as well as its healthy sibling because it is the one printed
// payload whose numbers a client author has to read AGAINST each other: the
// timestamp is fresh, the eta is fresh, the progress is a held value that looks
// like any other, and only the gap between asOf and the envelope says the card
// has stopped learning. A document that got those four numbers mutually
// consistent by accident would teach exactly the wrong lesson.
const contractExampleGoneQuiet = `{
      "v": 1,
      "status": "enroute",
      "eta": 1785535620,
      "vehicleName": "Blue Whale",
      "destination": "Home",
      "progress": 0.62,
      "asOf": 1785534930
    }`

// contractExampleGoneQuietTimestamp is that example's `aps.timestamp`.
const contractExampleGoneQuietTimestamp = 1785535380

func TestContractPrintedExampleDecodes(t *testing.T) {
	var state ActivityContentState
	dec := json.NewDecoder(strings.NewReader(contractExampleWithProgress))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		t.Fatalf("the contract's printed content-state does not decode: %v", err)
	}

	if state.Version != ActivityContentStateVersion {
		t.Errorf("v = %d, want %d", state.Version, ActivityContentStateVersion)
	}
	if state.Status != "enroute" {
		t.Errorf("status = %q, want enroute", state.Status)
	}
	if state.ETA == nil || *state.ETA != 1785535200 {
		t.Errorf("eta = %s, want 1785535200", fmtPtr(state.ETA))
	}
	if state.Progress == nil || *state.Progress != 0.62 {
		t.Errorf("progress = %s, want 0.62", fmtPtr(state.Progress))
	}
	if state.AsOf == nil || *state.AsOf != 1785534930 {
		t.Errorf("asOf = %s, want 1785534930", fmtPtr(state.AsOf))
	}

	// And back out again unchanged — the field is on the wire in both
	// directions, which is what a client's own round-trip test will assume.
	out, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reread map[string]any
	if err := json.Unmarshal(out, &reread); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got := reread["progress"]; got != 0.62 {
		t.Errorf("round-tripped progress = %v, want 0.62", got)
	}
}

// TestActivityPayloadCarriesProgressKey asserts the raw key and its shape on a
// real send.
func TestActivityPayloadCarriesProgressKey(t *testing.T) {
	n := testActivityNotification()
	p := 0.625
	n.ContentState.Progress = &p

	got := captureActivity(t, n)

	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	state, ok := payload["aps"].(map[string]any)["content-state"].(map[string]any)
	if !ok {
		t.Fatalf("content-state is not an object")
	}

	wantKeys := []string{"v", "status", "eta", "vehicleName", "destination", "progress", "asOf"}
	if got := keysOf(state); !sameSet(got, wantKeys) {
		t.Errorf("content-state keys = %v, want %v", got, wantKeys)
	}
	if got := state["progress"]; got != 0.625 {
		t.Errorf("content-state.progress = %v (%T), want the number 0.625", got, got)
	}
}

// TestActivityPayloadOmitsUnknownProgress is the byte-stability assertion an
// old installed build depends on: with nothing to say about the car's position
// the payload carries no `progress` key at all — no `progress: null` and no
// `progress: 0`, either of which the redesigned widget would render as a car
// that has not moved.
//
// The v3 card added `asOf` beside it, so the "nothing to say" payload is six
// keys rather than five now. The claim the test is making is unchanged and is
// the one that matters to an installed build: the five that shipped before r16
// are all present, all unmoved, and nothing among them changed type.
func TestActivityPayloadOmitsUnknownProgress(t *testing.T) {
	got := captureActivity(t, testActivityNotification())

	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	state := payload["aps"].(map[string]any)["content-state"].(map[string]any)

	if _, present := state["progress"]; present {
		t.Errorf("content-state.progress = %v, want the key absent entirely", state["progress"])
	}
	wantKeys := []string{"v", "status", "eta", "vehicleName", "destination", "asOf"}
	if got := keysOf(state); !sameSet(got, wantKeys) {
		t.Errorf("content-state keys = %v, want %v", got, wantKeys)
	}
}

// TestActivityPayloadOmitsUnsetAsOf applies `progress`'s own honesty rule to
// the new field: a content-state that was never stamped must omit the key
// rather than serialise the zero value.
//
// A pointer is what buys this, and it is worth a test rather than a comment.
// The zero of an int64 unix timestamp is not a neutral placeholder — it is a
// claim that the data was true in January 1970 — and the client renders `asOf`
// as a wall-clock time, so the failure would reach a rider's lock screen as
// "Last updated 12:00 AM" on an otherwise healthy card.
func TestActivityPayloadOmitsUnsetAsOf(t *testing.T) {
	n := testActivityNotification()
	n.ContentState.AsOf = nil

	got := captureActivity(t, n)

	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	state := payload["aps"].(map[string]any)["content-state"].(map[string]any)

	if _, present := state["asOf"]; present {
		t.Errorf("content-state.asOf = %v, want the key absent entirely", state["asOf"])
	}
}

// TestActivityNotifierPersistsProgressOnlyAfterDelivery is the monotonicity
// promise seen from the store side. The floor the next push clamps to must
// record what the phone HAS SEEN: advancing it on a push Apple never accepted
// would let the following value come in lower than the previous one.
func TestActivityNotifierPersistsProgressOnlyAfterDelivery(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	rc := RideContext{
		Status:             "accepted",
		VehicleName:        "Blue Whale",
		Destination:        "Home",
		ETAMinutes:         intPtr(6),
		TripMilesRemaining: miles(4),
		NavUpdatedAt:       &fresh,
		DispatchUnderway:   true,
	}

	t.Run("delivered", func(t *testing.T) {
		n, sender, store := newTestActivityNotifier(t, nil)
		store.context[activityRideID] = rc

		n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
			RideRequestID: activityRideID, RiderID: testRiderID, Status: "accepted",
		}))
		n.Wait()

		if len(sender.Sent()) != 1 {
			t.Fatalf("sends = %d, want 1", len(sender.Sent()))
		}
		saved := store.savedProgressFor(activityRideID, testRiderID)
		if len(saved) != 1 {
			t.Fatalf("persisted anchors = %d, want 1", len(saved))
		}
		// Reading/ReadingAt date the observation the fraction came from, so
		// the next push can tell a car that is quiet from a row that is busy.
		want := ProgressAnchor{
			Leg: ProgressLegPickup, Source: ProgressSourceNavDistance,
			Baseline: 4, Reading: 4, ReadingAt: fixedNow,
		}
		if !sameAnchor(saved[0], want) {
			t.Errorf("anchor = %+v, want %+v", saved[0], want)
		}
	})

	t.Run("refused by apple", func(t *testing.T) {
		n, sender, store := newTestActivityNotifier(t, nil)
		store.context[activityRideID] = rc
		sender.Err = errTestSendFailed

		n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
			RideRequestID: activityRideID, RiderID: testRiderID, Status: "accepted",
		}))
		n.Wait()

		if saved := store.savedProgressFor(activityRideID, testRiderID); len(saved) != 0 {
			t.Errorf("persisted %d anchors for an undelivered push, want none: %+v", len(saved), saved)
		}
	})
}

// TestActivityTickerAdvancesTheTrack drives two ticker passes over one leg and
// asserts the second push carries a HIGHER fraction than the first — the
// round trip through the store's anchor, which is the part a pure unit test on
// computeProgress cannot see.
func TestActivityTickerAdvancesTheTrack(t *testing.T) {
	fresh := fixedNow.Add(-30 * time.Second)
	n, sender, store := newTestActivityNotifier(t, nil)
	store.legs = []ActivityLeg{{
		Activity: Activity{RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken},
		RideContext: RideContext{
			Status:             "enroute",
			VehicleName:        "Blue Whale",
			Destination:        "Home",
			TripMilesRemaining: miles(10),
			NavUpdatedAt:       &fresh,
		},
	}}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	ticker.RunPass(t.Context())

	// The car covers six miles before the next pass.
	store.legs[0].TripMilesRemaining = miles(4)
	ticker.RunPass(t.Context())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sends = %d, want 2", len(sent))
	}
	if sent[0].ContentState.Progress == nil || *sent[0].ContentState.Progress != 0 {
		t.Errorf("first tick progress = %s, want 0", fmtPtr(sent[0].ContentState.Progress))
	}
	if sent[1].ContentState.Progress == nil || *sent[1].ContentState.Progress != 0.6 {
		t.Errorf("second tick progress = %s, want 0.6", fmtPtr(sent[1].ContentState.Progress))
	}
}
