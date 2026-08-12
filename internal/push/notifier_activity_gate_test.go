package push

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// The duplicate-banner gate's behaviour matrix (MYR-413).

// fakeActivityPresence answers HasLiveActivity from a fixed set of live
// (ride, user) pairs, and records what it was asked.
//
// Keyed on the PAIR rather than on the ride, which is the whole point of the
// fixture: the owner and the rider both receive pushes about one ride and only
// the rider has a card, so a fake keyed on the ride alone would make the
// owner-unaffected case pass for the wrong reason.
type fakeActivityPresence struct {
	mu   sync.Mutex
	live map[[2]string]bool
	err  error
	asks [][2]string
}

func newFakeActivityPresence(pairs ...[2]string) *fakeActivityPresence {
	f := &fakeActivityPresence{live: map[[2]string]bool{}}
	for _, p := range pairs {
		f.live[p] = true
	}
	return f
}

func (f *fakeActivityPresence) HasLiveActivity(_ context.Context, rideID, userID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks = append(f.asks, [2]string{rideID, userID})
	if f.err != nil {
		return false, f.err
	}
	return f.live[[2]string{rideID, userID}], nil
}

func (f *fakeActivityPresence) asked() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.asks...)
}

// riderCard is the pair a rider watching this ride's Live Activity presents.
func riderCard() [2]string { return [2]string{testRideID, testRiderID} }

// newGatedNotifier wires the standard two-party device registry against a given
// presence store.
func newGatedNotifier(t *testing.T, sender Sender, presence ActivityPresenceStore) *Notifier {
	t.Helper()
	devices := newFakeDeviceStore()
	devices.byUser[testOwnerID] = []Device{{Token: ownerDevice}}
	devices.byUser[testRiderID] = []Device{{Token: riderDevice, Sandbox: true}}

	return NewNotifier(sender, devices, nil, presence, &fakeVehicleNamer{name: "Blue Whale"},
		Config{Enabled: true}, discardLogger())
}

// TestLifecycleBannerSuppressionMatrix is the whole rule in one table: which
// transitions defer to the island, and which keep their banner whatever the
// registry says.
//
// The two rows that matter most are the NEGATIVE ones. `declined` with a live
// card must still send, because the unhappy endings are outside the design's
// six phases and expand no island — suppressing them would leave the worst news
// of a ride to a surface that never announced it. And a TOMBSTONED card must
// still send, because a rider who swiped the Activity away has no card left to
// read and must not be left dark.
func TestLifecycleBannerSuppressionMatrix(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		presence *fakeActivityPresence
		wantSent bool
		why      string
	}{
		{
			name:     "arrived with a live card is skipped",
			status:   "arrived",
			presence: newFakeActivityPresence(riderCard()),
			wantSent: false,
			why:      "the island expands with the same news and the banner covers it",
		},
		{
			name:     "accepted with a live card is skipped",
			status:   "accepted",
			presence: newFakeActivityPresence(riderCard()),
			wantSent: false,
			why:      "accepted is rung 2 of the ladder",
		},
		{
			name:     "arrived with a tombstoned card still sends",
			status:   "arrived",
			presence: newFakeActivityPresence(),
			wantSent: true,
			why:      "a swiped-away card must not leave the rider dark",
		},
		{
			name:     "arrived with no registration at all still sends",
			status:   "arrived",
			presence: newFakeActivityPresence(),
			wantSent: true,
			why:      "most rides are booked from a browser and have no Activity",
		},
		{
			name:     "declined always sends, card or no card",
			status:   "declined",
			presence: newFakeActivityPresence(riderCard()),
			wantSent: true,
			why:      "declined is outside the six phases; its Activity update alerts nothing",
		},
		{
			name:     "a lookup error sends the banner anyway",
			status:   "arrived",
			presence: &fakeActivityPresence{err: errors.New("database is down")},
			wantSent: true,
			why:      "the gate fails OPEN; a hiccup must not silence both surfaces at once",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newGatedNotifier(t, sender, tt.presence)

			n.handleStatusChanged(statusEvent(tt.status))
			n.Wait()

			sent := sender.Sent()
			if gotSent := len(sent) > 0; gotSent != tt.wantSent {
				t.Fatalf("sent %d banners, want sent=%v — %s", len(sent), tt.wantSent, tt.why)
			}
			if tt.wantSent && sent[0].DeviceToken != riderDevice {
				t.Errorf("banner went to %q, want the RIDER's device", sent[0].DeviceToken)
			}
		})
	}
}

// TestSuppressionIsScopedToTheRecipientNotTheRide is the seam the issue called
// out by name: the owner and the rider both get pushes on one ride, and only
// the rider has a card.
//
// The rider-only scoping is not a rule anywhere in the notifier — it falls out
// of the row being keyed (ride, user). This test is what proves that, by
// letting the rider's live card sit in the registry while the owner's own
// notification goes out untouched.
func TestSuppressionIsScopedToTheRecipientNotTheRide(t *testing.T) {
	sender := NewFakeSender()
	presence := newFakeActivityPresence(riderCard())
	n := newGatedNotifier(t, sender, presence)

	// The owner's "somebody wants a ride" push, on a ride whose RIDER has a
	// live card.
	n.handleCreated(createdEvent(strptr("Ada"), nil))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d notifications, want the owner's 1 — the rider's card "+
			"must not silence the owner's approval prompt", len(sent))
	}
	if sent[0].DeviceToken != ownerDevice {
		t.Errorf("device = %q, want the OWNER's", sent[0].DeviceToken)
	}
	// And the registry was never consulted: the created push is not gated at
	// all, so it costs no read. This is also the guard on the self-ride case —
	// an owner who rides their own car is both parties, and a gate here would
	// let their own card swallow the prompt they need in order to accept.
	if asks := presence.asked(); len(asks) != 0 {
		t.Errorf("presence store was asked %v on the created push; that push is not gated", asks)
	}
}

// TestSuppressionAsksAboutTheRecipient pins WHICH pair the gate looks up. A
// lookup keyed on the owner (or on the ride alone) would suppress the wrong
// person's banner, and the outcome for a single-registration ride would look
// identical in every other assertion here.
func TestSuppressionAsksAboutTheRecipient(t *testing.T) {
	presence := newFakeActivityPresence()
	n := newGatedNotifier(t, NewFakeSender(), presence)

	n.handleStatusChanged(statusEvent("arrived"))
	n.Wait()

	asks := presence.asked()
	if len(asks) != 1 {
		t.Fatalf("presence store asked %d times, want exactly 1", len(asks))
	}
	if want := riderCard(); asks[0] != want {
		t.Errorf("asked about %v, want %v — the gate must key on the RECIPIENT", asks[0], want)
	}
}

// TestDeclinedNeverConsultsTheRegistry proves the ladder check runs BEFORE the
// store read rather than after it.
//
// Behaviourally the two orders agree, so this is a cost assertion: `declined`
// can never be suppressed, and a read issued to reach that conclusion is a
// database round-trip on the notification path buying nothing.
func TestDeclinedNeverConsultsTheRegistry(t *testing.T) {
	presence := newFakeActivityPresence(riderCard())
	n := newGatedNotifier(t, NewFakeSender(), presence)

	n.handleStatusChanged(statusEvent("declined"))
	n.Wait()

	if asks := presence.asked(); len(asks) != 0 {
		t.Errorf("presence store was asked %v on a declined transition", asks)
	}
}

// TestDueBannerSurvivesALiveCard covers the third fan-out site. The reservation
// sweeper's "your car is heading your way" rides its own topic and moves no
// ride status, so the ladder never advances and no island expands for it — a
// gate there would delete the notification rather than defer to a better one.
func TestDueBannerSurvivesALiveCard(t *testing.T) {
	sender := NewFakeSender()
	n := newGatedNotifier(t, sender, newFakeActivityPresence(riderCard()))

	n.handleDue(dueEvent())
	n.Wait()

	// MYR-535: ride.due wakes both parties, so the count is 2. What this test
	// pins is that the RIDER's banner is among them — the gate must not
	// suppress it, because the due push is not a phase the island announces.
	sent := sender.Sent()
	if got := len(sent); got != 2 {
		t.Fatalf("sent %d notifications on ride.due, want 2 (rider + owner)", got)
	}
	riderSeen := false
	for i := range sent {
		if sent[i].DeviceToken == riderDevice {
			riderSeen = true
		}
	}
	if !riderSeen {
		t.Error("the rider's due banner was suppressed — it is not a phase the island announces")
	}
}

// TestUnwiredPresenceStoreSuppressesNothing pins the fail-open default. A nil
// store is the pre-MYR-413 behaviour, and it is what every other test file in
// this package still constructs.
func TestUnwiredPresenceStoreSuppressesNothing(t *testing.T) {
	sender := NewFakeSender()
	n := newGatedNotifier(t, sender, nil)

	n.handleStatusChanged(statusEvent("arrived"))
	n.Wait()

	if got := len(sender.Sent()); got != 1 {
		t.Errorf("sent %d banners with no presence store wired, want 1", got)
	}
}

// TestAlertLadderStatusesMatchTheAlertPhases pins the MYR-413 suppression set
// against the MYR-398 ladder it is derived from.
//
// The two live in different files and cannot share a constant — alertPhaseFor
// needs the car's ETA to tell Enroute from Arriving, while this set only needs
// to know whether a status alerts at all — so this is the seam where they are
// held together. A seventh rung added to the ladder without a matching entry
// here would silently resume double-notifying on that phase.
func TestAlertLadderStatusesMatchTheAlertPhases(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{status: statusRequested, want: true},
		{status: statusAccepted, want: true},
		{status: statusArrived, want: true},
		{status: statusEnroute, want: true},
		{status: statusCompleted, want: true},
		// The unhappy endings are outside the design's six: their Activity
		// update expands no island, so their banner is the only news there is.
		{status: statusDeclined, want: false},
		{status: "cancelled", want: false},
		{status: "reservation_expired", want: false},
		{status: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := carriesIslandAlert(tt.status); got != tt.want {
				t.Errorf("carriesIslandAlert(%q) = %v, want %v", tt.status, got, tt.want)
			}
			// And the derivation itself: a status this set claims alerts must
			// map to a real rung of the ladder, and one it claims does not must
			// map to none.
			phase := alertPhaseFor(RideContext{Status: tt.status}, fixedNow)
			if hasPhase := phase != AlertPhaseNone; hasPhase != tt.want {
				t.Errorf("alertPhaseFor(%q) = %d but carriesIslandAlert says %v — "+
					"the ladder and the suppression set have drifted", tt.status, phase, tt.want)
			}
		})
	}
}
