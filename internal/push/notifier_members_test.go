package push

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-540 — the group fan-out: a member is a passenger, so every RIDER-flavoured
// push reaches them too, in the same words.

const (
	testMemberID   = "cmember001"
	testMemberIDB  = "cmember002"
	memberDevice   = "dev-member-0001"
	memberDeviceB  = "dev-member-0002"
	fanOutTestRide = testRideID
)

// fakeRideMembers answers the delivery-time member lookup.
type fakeRideMembers struct {
	ids   []string
	err   error
	calls int
}

func (f *fakeRideMembers) RideMemberIDs(context.Context, string) ([]string, error) {
	f.calls++
	return f.ids, f.err
}

// newGroupNotifier is newTestNotifier plus two joined members, each with a
// phone, and the lookup that resolves them.
func newGroupNotifier(t *testing.T, sender Sender, members *fakeRideMembers) *Notifier {
	t.Helper()
	devices := newFakeDeviceStore()
	devices.byUser[testOwnerID] = []Device{{Token: ownerDevice}}
	devices.byUser[testRiderID] = []Device{{Token: riderDevice, Sandbox: true}}
	devices.byUser[testMemberID] = []Device{{Token: memberDevice}}
	devices.byUser[testMemberIDB] = []Device{{Token: memberDeviceB}}

	return NewNotifier(sender, devices, nil, nil, &fakeVehicleNamer{name: "Blue Whale"},
		Config{Enabled: true}, discardLogger()).WithRideMembers(members)
}

// tokensOf collects the device tokens a fan-out reached.
func tokensOf(sent []Notification) map[string]string {
	out := make(map[string]string, len(sent))
	for _, s := range sent {
		out[s.DeviceToken] = s.Title
	}
	return out
}

// TestNotifierStatusChangeReachesMembers is the headline: an owner's ACCEPT
// wakes the requester and everybody riding with them, with the same copy.
func TestNotifierStatusChangeReachesMembers(t *testing.T) {
	sender := NewFakeSender()
	members := &fakeRideMembers{ids: []string{testMemberID, testMemberIDB}}
	n := newGroupNotifier(t, sender, members)

	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	got := tokensOf(sender.Sent())
	if len(got) != 3 {
		t.Fatalf("reached %d devices, want 3 (rider + two members): %+v", len(got), got)
	}
	rider, ok := got[riderDevice]
	if !ok {
		t.Fatal("the requester was not notified")
	}
	// THE SAME WORDS. A member is a passenger, and forking the copy would mean
	// inventing a second voice for one fact.
	for _, token := range []string{memberDevice, memberDeviceB} {
		title, ok := got[token]
		if !ok {
			t.Fatalf("member device %s was not notified", token)
		}
		if title != rider {
			t.Errorf("member copy = %q, rider copy = %q; they must be identical", title, rider)
		}
	}
}

// TestNotifierDueReachesMembers pins the one a group most needs: "your car is
// on its way" is what tells everybody to start walking outside.
func TestNotifierDueReachesMembers(t *testing.T) {
	sender := NewFakeSender()
	n := newGroupNotifier(t, sender, &fakeRideMembers{ids: []string{testMemberID}})

	n.handleDue(events.NewEvent(events.RideDueEvent{
		RideRequestID: fanOutTestRide, VehicleID: testVehicleID,
		RiderID: testRiderID, OwnerID: testOwnerID,
		ScheduledFor: time.Now(), DueAt: time.Now(),
	}))
	n.Wait()

	got := tokensOf(sender.Sent())
	if _, ok := got[memberDevice]; !ok {
		t.Fatalf("the member was not told the car was dispatched: %+v", got)
	}
	if got[memberDevice] != got[riderDevice] {
		t.Errorf("member copy = %q, rider copy = %q", got[memberDevice], got[riderDevice])
	}
}

// TestNotifierTripChangeExcludesTheEditingMember pins the standing rule: nobody
// is told about their own thumb, whichever role the thumb belongs to.
func TestNotifierTripChangeExcludesTheEditingMember(t *testing.T) {
	sender := NewFakeSender()
	n := newGroupNotifier(t, sender, &fakeRideMembers{ids: []string{testMemberID, testMemberIDB}})

	n.handleTripChanged(events.NewEvent(events.RideTripChangedEvent{
		RideRequestID: fanOutTestRide, VehicleID: testVehicleID,
		RiderID: testRiderID, OwnerID: testOwnerID,
		// A MEMBER made the edit — the full-collaboration case.
		EditorUserID: testMemberID, Status: "enroute",
		NewDropoff: &events.RidePlace{Latitude: 1, Longitude: 2, Label: "Secret Place"},
		UpdatedAt:  time.Now(),
	}))
	n.Wait()

	got := tokensOf(sender.Sent())
	if _, ok := got[memberDevice]; ok {
		t.Error("the editing member was told about their own edit")
	}
	if _, ok := got[memberDeviceB]; !ok {
		t.Errorf("the other member was not told: %+v", got)
	}
	// Both standing parties hear it too — neither of them made this edit.
	for _, token := range []string{riderDevice, ownerDevice} {
		if _, ok := got[token]; !ok {
			t.Errorf("device %s was not told about a member's edit: %+v", token, got)
		}
	}
}

// TestNotifierMemberLookupFailureStillNotifiesTheRider pins the failure
// direction. Abandoning the whole fan-out because one lookup failed would trade
// a missed member notification for a missed RIDER notification — the one this
// system exists to deliver.
func TestNotifierMemberLookupFailureStillNotifiesTheRider(t *testing.T) {
	sender := NewFakeSender()
	n := newGroupNotifier(t, sender, &fakeRideMembers{err: errors.New("boom")})

	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	got := tokensOf(sender.Sent())
	if _, ok := got[riderDevice]; !ok {
		t.Fatalf("the requester lost their push to a member-lookup failure: %+v", got)
	}
	if len(got) != 1 {
		t.Errorf("reached %d devices, want 1: %+v", len(got), got)
	}
}

// TestNotifierUnwiredMembersIsPreMYR540Behaviour pins that a deployment without
// the lookup behaves exactly as it did before this issue.
func TestNotifierUnwiredMembersIsPreMYR540Behaviour(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	if got := tokensOf(sender.Sent()); len(got) != 1 {
		t.Fatalf("reached %d devices, want 1 (the rider alone): %+v", len(got), got)
	}
}

// TestNotifierMemberFanOutExcludesTheStandingParties pins the belt-and-braces
// de-duplication. The join endpoint already 409s the requester and the owner, so
// this should never fire — but "should never" is not what a fan-out should rest
// on when the cost of being wrong is a phone buzzing twice.
func TestNotifierMemberFanOutExcludesTheStandingParties(t *testing.T) {
	sender := NewFakeSender()
	// A membership row that should not exist: the requester, listed as a member.
	n := newGroupNotifier(t, sender, &fakeRideMembers{ids: []string{testRiderID, testOwnerID, testMemberID}})

	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	counts := map[string]int{}
	for _, s := range sender.Sent() {
		counts[s.DeviceToken]++
	}
	if counts[riderDevice] != 1 {
		t.Errorf("the requester got %d copies, want exactly 1", counts[riderDevice])
	}
	if counts[memberDevice] != 1 {
		t.Errorf("the member got %d copies, want exactly 1", counts[memberDevice])
	}
}
