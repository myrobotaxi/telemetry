package push

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// MYR-349 — the preference gate.
//
// These tests exist because the thing being fixed was NOT a missing feature but
// a LIE: the Settings switches moved and nothing anywhere read them. So the
// assertions are all about the SEND, never about the stored value — a test that
// only proved the row round-tripped would be green on the exact defect this
// issue is about.
//
// Written failing-first, then proven to be real guards by reverting the gate:
// see the PR body for the raw before/after counts.

// fakePrefStore is the in-memory PrefStore double.
//
// Mutex-guarded because the gate runs inside the notifier's bounded workers and
// a test that fires several events sees several concurrent lookups — the same
// reason fakeDeviceStore carries one.
type fakePrefStore struct {
	mu     sync.Mutex
	byUser map[string]Prefs
	// err, when non-nil, is returned for every lookup — the fail-open path.
	err error
	// calls counts lookups, so a test can prove the gate ran (or was skipped).
	calls int
}

func newFakePrefStore() *fakePrefStore {
	return &fakePrefStore{byUser: map[string]Prefs{}}
}

// PrefsForUser mirrors the contract the real store keeps: an account with no
// stored row yields DefaultPrefs and a nil error, never a zero value.
func (f *fakePrefStore) PrefsForUser(_ context.Context, userID string) (Prefs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return Prefs{}, f.err
	}
	if p, ok := f.byUser[userID]; ok {
		return p, nil
	}
	return DefaultPrefs(), nil
}

// lookups returns the call count under the lock.
func (f *fakePrefStore) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// notifierWithPrefs wires a notifier over the shared device fixtures plus a
// preference store.
func notifierWithPrefs(t *testing.T, prefs PrefStore) (*Notifier, *FakeSender) {
	t.Helper()
	devices := newFakeDeviceStore()
	devices.byUser[testOwnerID] = []Device{{Token: ownerDevice}}
	devices.byUser[testRiderID] = []Device{{Token: riderDevice, Sandbox: true}}

	sender := NewFakeSender()
	n := NewNotifier(sender, devices, prefs, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())
	return n, sender
}

// TestPrefs_AllowsEveryCategoryByDefault pins the default in the one place the
// whole design rests on. Every other guarantee in this file assumes an account
// that has never opened Settings receives everything; if this inverted, MYR-349
// would silence the platform on the day it shipped.
func TestPrefs_AllowsEveryCategoryByDefault(t *testing.T) {
	def := DefaultPrefs()
	for _, c := range Categories() {
		if !def.Allows(c) {
			t.Errorf("DefaultPrefs().Allows(%q) = false, want true — an account that has "+
				"never touched a switch must receive every category", c)
		}
	}
}

// TestPrefs_ZeroValueIsNotTheDefault is the companion guard: the zero value is
// "everything off", which is why nothing may build a Prefs literally.
func TestPrefs_ZeroValueIsNotTheDefault(t *testing.T) {
	var zero Prefs
	for _, c := range Categories() {
		if zero.Allows(c) {
			t.Errorf("Prefs{}.Allows(%q) = true, want false — the zero value must not "+
				"masquerade as the default, or a forgotten assignment would read as consent", c)
		}
	}
}

// TestPrefs_AllowsIsExhaustive walks every category and proves Allows reads the
// matching field — a copy-paste in that switch would silence the wrong switch,
// which no send-path test would notice because all five look alike.
func TestPrefs_AllowsIsExhaustive(t *testing.T) {
	for _, c := range Categories() {
		t.Run(string(c), func(t *testing.T) {
			// Start from everything OFF and turn on exactly this one.
			var only Prefs
			switch c {
			case CategoryRideLifecycle:
				only.RideLifecycle = true
			case CategoryDriveStarted:
				only.DriveStarted = true
			case CategoryDriveCompleted:
				only.DriveCompleted = true
			case CategoryChargingComplete:
				only.ChargingComplete = true
			case CategoryViewerJoined:
				only.ViewerJoined = true
			default:
				t.Fatalf("category %q has no field mapping in this test — a new category "+
					"was added without teaching the gate about it", c)
			}

			if !only.Allows(c) {
				t.Errorf("Allows(%q) = false with only that field set — the switch reads "+
					"the wrong field", c)
			}
			for _, other := range Categories() {
				if other == c {
					continue
				}
				if only.Allows(other) {
					t.Errorf("Allows(%q) = true with only %q set — one category's switch "+
						"is answering for another", other, c)
				}
			}
		})
	}
}

// TestPrefs_UnknownCategoryFailsOpen pins the deliberate default arm. A send
// site added with a category the model has no column for must still deliver.
func TestPrefs_UnknownCategoryFailsOpen(t *testing.T) {
	var allOff Prefs
	if !allOff.Allows(Category("something_new")) {
		t.Error("Allows(unknown) = false, want true — an uncategorized send site must " +
			"deliver loudly, not vanish silently for every user")
	}
}

// TestNotifier_RideLifecyclePrefOffSendsNothing is the issue's headline
// assertion, across all three fan-out sites.
//
// Every ride-lifecycle event is driven through the REAL bus handler, not
// fanOut directly, so the category actually travels from the send site to the
// gate — a test that called fanOut with a hand-written delivery would pass even
// if a handler forgot to name its category at all.
func TestNotifier_RideLifecyclePrefOffSendsNothing(t *testing.T) {
	// The recipient differs per topic: `ride.request.created` wakes the OWNER,
	// the other two wake the RIDER. The gate must read the RECIPIENT's row.
	cases := []struct {
		name      string
		recipient string
		deliver   func(n *Notifier)
	}{
		{
			name:      "ride.request.created (owner)",
			recipient: testOwnerID,
			deliver:   func(n *Notifier) { n.handleCreated(createdEvent(strptr("Sam"), nil)) },
		},
		{
			name:      "ride.status.changed accepted (rider)",
			recipient: testRiderID,
			deliver:   func(n *Notifier) { n.handleStatusChanged(statusEvent("accepted")) },
		},
		{
			name:      "ride.status.changed declined (rider)",
			recipient: testRiderID,
			deliver:   func(n *Notifier) { n.handleStatusChanged(statusEvent("declined")) },
		},
		{
			name:      "ride.status.changed arrived (rider)",
			recipient: testRiderID,
			deliver:   func(n *Notifier) { n.handleStatusChanged(statusEvent("arrived")) },
		},
		{
			name:      "ride.due (rider)",
			recipient: testRiderID,
			deliver:   func(n *Notifier) { n.handleDue(dueEvent()) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prefs := newFakePrefStore()
			off := DefaultPrefs()
			off.RideLifecycle = false
			prefs.byUser[tc.recipient] = off

			n, sender := notifierWithPrefs(t, prefs)
			tc.deliver(n)
			n.Wait()

			if got := sender.Sent(); len(got) != 0 {
				t.Errorf("sent %d notification(s) with ride_lifecycle OFF, want 0 — the "+
					"switch is decorative again: %+v", len(got), got)
			}
			if prefs.lookups() == 0 {
				t.Error("the preference store was never consulted — the gate is not on this path")
			}
		})
	}
}

// TestNotifier_RideLifecyclePrefOnStillSends is the other half: the gate must
// not be a blanket mute. Same five paths, preference explicitly ON.
func TestNotifier_RideLifecyclePrefOnStillSends(t *testing.T) {
	prefs := newFakePrefStore()
	on := DefaultPrefs()
	prefs.byUser[testOwnerID] = on
	prefs.byUser[testRiderID] = on

	n, sender := notifierWithPrefs(t, prefs)
	n.handleCreated(createdEvent(strptr("Sam"), nil))
	n.handleStatusChanged(statusEvent("accepted"))
	n.handleStatusChanged(statusEvent("declined"))
	n.handleStatusChanged(statusEvent("arrived"))
	n.handleDue(dueEvent())
	n.Wait()

	if got := sender.Sent(); len(got) != 5 {
		t.Errorf("sent %d notification(s) with ride_lifecycle ON, want 5", len(got))
	}
}

// TestNotifier_AbsentRowSends is the migration-day guarantee. Nobody has a row
// on the day this ships, and the missing row must be indistinguishable from
// "all on" — otherwise deploying MYR-349 silences every notification at once.
func TestNotifier_AbsentRowSends(t *testing.T) {
	prefs := newFakePrefStore() // empty map: nobody has a row

	n, sender := notifierWithPrefs(t, prefs)
	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	if got := sender.Sent(); len(got) != 1 {
		t.Errorf("sent %d notification(s) for an account with NO stored prefs, want 1 — "+
			"a missing row is not a silenced user", len(got))
	}
	if got := prefs.lookups(); got != 1 {
		t.Errorf("prefs lookups = %d, want 1", got)
	}
}

// TestNotifier_PrefLookupFailureSends pins the fail-open decision. A database
// hiccup must not become platform-wide silence, because nothing about a ride
// waits on push and no error would surface anywhere.
func TestNotifier_PrefLookupFailureSends(t *testing.T) {
	prefs := newFakePrefStore()
	prefs.err = errors.New("connection refused")

	n, sender := notifierWithPrefs(t, prefs)
	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	if got := sender.Sent(); len(got) != 1 {
		t.Errorf("sent %d notification(s) when the prefs lookup ERRORED, want 1 — the "+
			"gate must fail open; failing closed turns a transient DB error into "+
			"a rider standing on a sidewalk with no signal anywhere", len(got))
	}
}

// TestNotifier_NilPrefStoreSends covers the unwired deploy — a build where the
// preference store was never passed. Same direction as every other failure.
func TestNotifier_NilPrefStoreSends(t *testing.T) {
	n, sender := notifierWithPrefs(t, nil)
	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	if got := sender.Sent(); len(got) != 1 {
		t.Errorf("sent %d notification(s) with NO PrefStore wired, want 1", len(got))
	}
}

// TestNotifier_OtherCategoriesOffDoNotSilenceRideLifecycle proves the gate is
// per-category rather than "any switch off mutes everything". This is the
// failure mode a single shared boolean would produce, and it would look correct
// in any test that only ever turned one switch off at a time.
func TestNotifier_OtherCategoriesOffDoNotSilenceRideLifecycle(t *testing.T) {
	prefs := newFakePrefStore()
	prefs.byUser[testRiderID] = Prefs{
		RideLifecycle:    true,
		DriveStarted:     false,
		DriveCompleted:   false,
		ChargingComplete: false,
		ViewerJoined:     false,
	}

	n, sender := notifierWithPrefs(t, prefs)
	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	if got := sender.Sent(); len(got) != 1 {
		t.Errorf("sent %d notification(s) with the four NON-ride categories off, want 1 — "+
			"the gate is reading the wrong column or collapsing all five", len(got))
	}
}

// TestNotifier_GateReadsTheRecipientNotTheOtherParty is the subtle one. A ride
// has two people; `ride.request.created` wakes the OWNER while the other two
// topics wake the RIDER. Reading the wrong party's row would honour a
// preference the recipient never set — and would be invisible to any test where
// both parties happen to agree.
func TestNotifier_GateReadsTheRecipientNotTheOtherParty(t *testing.T) {
	t.Run("owner silenced, rider on → created sends nothing", func(t *testing.T) {
		prefs := newFakePrefStore()
		ownerOff := DefaultPrefs()
		ownerOff.RideLifecycle = false
		prefs.byUser[testOwnerID] = ownerOff
		prefs.byUser[testRiderID] = DefaultPrefs()

		n, sender := notifierWithPrefs(t, prefs)
		n.handleCreated(createdEvent(strptr("Sam"), nil))
		n.Wait()

		if got := sender.Sent(); len(got) != 0 {
			t.Errorf("sent %d to the OWNER who silenced ride_lifecycle, want 0 — the gate "+
				"is reading the rider's row", len(got))
		}
	})

	t.Run("rider silenced, owner on → created still sends", func(t *testing.T) {
		prefs := newFakePrefStore()
		riderOff := DefaultPrefs()
		riderOff.RideLifecycle = false
		prefs.byUser[testOwnerID] = DefaultPrefs()
		prefs.byUser[testRiderID] = riderOff

		n, sender := notifierWithPrefs(t, prefs)
		n.handleCreated(createdEvent(strptr("Sam"), nil))
		n.Wait()

		if got := sender.Sent(); len(got) != 1 {
			t.Errorf("sent %d to the OWNER whose own switch is ON, want 1 — the RIDER's "+
				"preference is silencing the owner's notification", len(got))
		}
	})
}

// TestNotifier_SilencedCategorySkipsTheDeviceLookup pins the ordering. Cheap,
// but the point is behavioural rather than performance: a fan-out that resolved
// devices first would log "push sent … delivered=0" for a silenced category,
// which reads in production like a delivery failure rather than a preference.
func TestNotifier_SilencedCategorySkipsTheDeviceLookup(t *testing.T) {
	devices := newFakeDeviceStore()
	devices.byUser[testRiderID] = []Device{{Token: riderDevice}}

	prefs := newFakePrefStore()
	off := DefaultPrefs()
	off.RideLifecycle = false
	prefs.byUser[testRiderID] = off

	sender := NewFakeSender()
	n := NewNotifier(sender, devices, prefs, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())
	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	devices.mu.Lock()
	listCalls := devices.listCalls
	devices.mu.Unlock()
	if listCalls != 0 {
		t.Errorf("device lookups = %d, want 0 — a silenced category must not fan out", listCalls)
	}
}
