package push

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// --- fakes -----------------------------------------------------------------

// fakeDeviceStore serves a fixed device list per user and records deletions.
type fakeDeviceStore struct {
	mu      sync.Mutex
	byUser  map[string][]Device
	listErr error
	deleted []string
	// listCalls counts DevicesForUser calls. MYR-349 asserts on it to prove a
	// preference-silenced category never reaches the fan-out at all.
	listCalls int
}

func newFakeDeviceStore() *fakeDeviceStore {
	return &fakeDeviceStore{byUser: map[string][]Device{}}
}

func (f *fakeDeviceStore) DevicesForUser(_ context.Context, userID string) ([]Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byUser[userID], nil
}

func (f *fakeDeviceStore) DeleteDeviceToken(_ context.Context, deviceToken string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, deviceToken)
	return nil
}

func (f *fakeDeviceStore) deletedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// fakeVehicleNamer returns a fixed nickname (or an error).
type fakeVehicleNamer struct {
	name string
	err  error
}

// fakeRequesterNamer stands in for the MYR-535 rider-first-name resolver.
type fakeRequesterNamer struct {
	name string
	err  error
}

func (f *fakeRequesterNamer) RequesterFirstName(context.Context, string) (string, error) {
	return f.name, f.err
}

func (f *fakeVehicleNamer) VehicleName(context.Context, string) (string, error) {
	return f.name, f.err
}

// --- helpers ---------------------------------------------------------------

const (
	testOwnerID   = "cowner001"
	testRiderID   = "crider001"
	testRideID    = "cride001"
	testVehicleID = "cveh001"
	ownerDevice   = "dev-owner-0001"
	riderDevice   = "dev-rider-0001"
)

// newTestNotifier wires a notifier whose owner and rider each have one device.
//
// The PrefStore is deliberately nil here (MYR-349): every test in this file
// predates preferences and asserts the pre-MYR-349 behaviour, and nil is
// exactly that — every category on. The gate's own coverage lives in
// prefs_test.go, which wires a real store. Keeping these nil is also the
// regression guard that matters most: if the gate ever started failing CLOSED
// on an unwired store, every assertion in this file would go red at once.
func newTestNotifier(t *testing.T, sender Sender, namer VehicleNamer) *Notifier {
	t.Helper()
	devices := newFakeDeviceStore()
	devices.byUser[testOwnerID] = []Device{{Token: ownerDevice}}
	devices.byUser[testRiderID] = []Device{{Token: riderDevice, Sandbox: true}}

	return NewNotifier(sender, devices, nil, nil, namer, Config{Enabled: true}, discardLogger())
}

func strptr(s string) *string { return &s }

func createdEvent(name *string, scheduledFor *time.Time) events.Event {
	return events.NewEvent(events.RideRequestCreatedEvent{
		RideRequestID: testRideID,
		VehicleID:     testVehicleID,
		RiderID:       testRiderID,
		OwnerID:       testOwnerID,
		Status:        "requested",
		RequesterName: name,
		ScheduledFor:  scheduledFor,
	})
}

func statusEvent(status string) events.Event {
	return events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: testRideID,
		VehicleID:     testVehicleID,
		RiderID:       testRiderID,
		OwnerID:       testOwnerID,
		Status:        status,
	})
}

// scheduledStatusEvent is statusEvent for a RESERVATION — the ride carries a
// reservation instant, which is what MYR-360's declined copy forks on.
func scheduledStatusEvent(status string, at time.Time) events.Event {
	return events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: testRideID,
		VehicleID:     testVehicleID,
		RiderID:       testRiderID,
		OwnerID:       testOwnerID,
		Status:        status,
		ScheduledFor:  &at,
	})
}

func dueEvent() events.Event {
	return events.NewEvent(events.RideDueEvent{
		RideRequestID: testRideID,
		VehicleID:     testVehicleID,
		RiderID:       testRiderID,
		OwnerID:       testOwnerID,
		ScheduledFor:  time.Now(),
		DueAt:         time.Now(),
	})
}

// --- audience + copy -------------------------------------------------------

// TestNotifierCreatedNotifiesOwner pins the audience (the OWNER, never the
// rider who just tapped the button) and the four title variants.
func TestNotifierCreatedNotifiesOwner(t *testing.T) {
	scheduled := time.Date(2026, 7, 31, 17, 30, 0, 0, time.UTC)

	tests := []struct {
		name         string
		requester    *string
		scheduledFor *time.Time
		wantTitle    string
	}{
		{name: "named on-demand", requester: strptr("Ada"), wantTitle: "Ada wants a ride"},
		{name: "named scheduled", requester: strptr("Ada"), scheduledFor: &scheduled, wantTitle: "Ada requested a scheduled ride"},
		{name: "anonymous on-demand", wantTitle: "New ride request"},
		{name: "anonymous scheduled", scheduledFor: &scheduled, wantTitle: "New scheduled ride request"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

			n.handleCreated(createdEvent(tt.requester, tt.scheduledFor))
			n.Wait()

			sent := sender.Sent()
			if len(sent) != 1 {
				t.Fatalf("sent %d notifications, want 1", len(sent))
			}
			if sent[0].DeviceToken != ownerDevice {
				t.Errorf("device = %q, want the OWNER's device", sent[0].DeviceToken)
			}
			if sent[0].Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", sent[0].Title, tt.wantTitle)
			}
			if sent[0].RideID != testRideID {
				t.Errorf("rideId = %q, want %q", sent[0].RideID, testRideID)
			}
		})
	}
}

// TestNotifierScheduledCopyOmitsTheTime is the explicit guard on the MYR-186
// scheduled-time decision: the server holds the reservation instant in UTC and
// knows no client time zone, so the notification must not render a time at all.
func TestNotifierScheduledCopyOmitsTheTime(t *testing.T) {
	scheduled := time.Date(2026, 7, 31, 17, 30, 0, 0, time.UTC)
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{})

	n.handleCreated(createdEvent(strptr("Ada"), &scheduled))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d notifications, want 1", len(sent))
	}
	copyText := sent[0].Title + " " + sent[0].Body
	for _, forbidden := range []string{"17:30", "5:30", "UTC", "Jul", "July", "31", "2026"} {
		if strings.Contains(copyText, forbidden) {
			t.Errorf("copy %q contains %q — scheduled pushes must not render a time", copyText, forbidden)
		}
	}
}

// TestNotifierStatusChanged covers which transitions speak, WHICH PARTY each
// one speaks to, which stay silent, and the copy for each.
//
// The audience column is the point of the table, not decoration. One
// `ride.status.changed` event feeds two different copy functions with two
// different recipients: the rider hears the OWNER's actions (accept, decline,
// arrive) and the owner hears the RIDER's one action (`enroute` — MYR-462).
// A transition that is silent is silent for BOTH, and the assertion below
// checks the total send count, so a regression that leaks a push to the wrong
// party fails here rather than in production.
func TestNotifierStatusChanged(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		vehicleName string
		wantDevice  string // "" means neither party is notified
		wantTitle   string
		// wantSandbox is the recipient device's own flag, which the sender must
		// receive verbatim. It tracks the fixture in newTestNotifier — the
		// rider's device is a sandbox one and the owner's is not — so this
		// column also proves the flag follows the DEVICE and not the send site.
		wantSandbox bool
	}{
		{name: "accepted reaches the rider", status: "accepted", vehicleName: "Blue Whale", wantDevice: riderDevice, wantTitle: "Your ride is confirmed", wantSandbox: true},
		{name: "declined names the car", status: "declined", vehicleName: "Blue Whale", wantDevice: riderDevice, wantTitle: "Blue Whale can't take this ride", wantSandbox: true},
		{name: "declined falls back", status: "declined", wantDevice: riderDevice, wantTitle: "Your car can't take this ride", wantSandbox: true},
		{name: "arrived reaches the rider", status: "arrived", wantDevice: riderDevice, wantTitle: "Your car is here — your turn to start", wantSandbox: true},
		{name: "enroute reaches the OWNER", status: "enroute", wantDevice: ownerDevice, wantTitle: "Your rider started the ride", wantSandbox: false},
		{name: "requested is silent", status: "requested"},
		{name: "completed is silent", status: "completed"},
		{name: "cancelled is silent", status: "cancelled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{name: tt.vehicleName})

			n.handleStatusChanged(statusEvent(tt.status))
			n.Wait()

			sent := sender.Sent()
			if tt.wantDevice == "" {
				if len(sent) != 0 {
					t.Fatalf("sent %d notifications for %q, want silence", len(sent), tt.status)
				}
				return
			}
			if len(sent) != 1 {
				t.Fatalf("sent %d notifications, want exactly 1", len(sent))
			}
			if sent[0].DeviceToken != tt.wantDevice {
				t.Errorf("device = %q, want %q", sent[0].DeviceToken, tt.wantDevice)
			}
			if sent[0].Sandbox != tt.wantSandbox {
				t.Errorf("sandbox = %v, want %v — the device row's flag must reach the sender", sent[0].Sandbox, tt.wantSandbox)
			}
			if sent[0].Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", sent[0].Title, tt.wantTitle)
			}
		})
	}
}

// TestNotifierEnrouteNamesTheRider is the MYR-462 copy path: when the event
// carries the rider's resolved display name the owner's banner uses it, and
// only the FIRST name, per the payload policy in copy.go.
func TestNotifierEnrouteNamesTheRider(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	name := "Aarthi Sivasankar"
	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: testRideID,
		VehicleID:     testVehicleID,
		RiderID:       testRiderID,
		OwnerID:       testOwnerID,
		Status:        "enroute",
		RequesterName: &name,
	}))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d notifications, want 1", len(sent))
	}
	if sent[0].DeviceToken != ownerDevice {
		t.Errorf("device = %q, want the OWNER's device", sent[0].DeviceToken)
	}
	if got, want := sent[0].Title, "Aarthi started the ride"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if strings.Contains(sent[0].Title, "Sivasankar") {
		t.Errorf("title = %q leaks the surname — copy is first-name-only", sent[0].Title)
	}
}

// TestNotifierEnrouteSilentWhenOwnerIsTheRider guards the self-ride case, which
// on this platform is the common one: an owner riding their own car must not be
// told they started a ride they are sitting in and started themselves.
func TestNotifierEnrouteSilentWhenOwnerIsTheRider(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: testRideID,
		VehicleID:     testVehicleID,
		RiderID:       testOwnerID, // one person, both roles
		OwnerID:       testOwnerID,
		Status:        "enroute",
	}))
	n.Wait()

	if sent := sender.Sent(); len(sent) != 0 {
		t.Fatalf("sent %d notifications to a self-rider, want silence (titles: %v)", len(sent), sent)
	}
}

// TestNotifierDeclinedReservationCopy is the MYR-360 end-to-end: an owner
// declining an ACCEPTED reservation must reach the rider's lock screen saying
// it is about their SCHEDULED ride, not as a reply to a request they just made
// — and still without a time, since the server knows no client time zone.
func TestNotifierDeclinedReservationCopy(t *testing.T) {
	scheduled := time.Date(2026, 8, 1, 17, 30, 0, 0, time.UTC)

	tests := []struct {
		name      string
		event     events.Event
		wantTitle string
	}{
		{name: "instant decline keeps its copy", event: statusEvent("declined"), wantTitle: "Blue Whale can't take this ride"},
		{name: "reservation decline names the scheduled ride", event: scheduledStatusEvent("declined", scheduled), wantTitle: "Blue Whale can't make your scheduled ride"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

			n.handleStatusChanged(tt.event)
			n.Wait()

			sent := sender.Sent()
			if len(sent) != 1 {
				t.Fatalf("sent %d notifications, want 1", len(sent))
			}
			if sent[0].DeviceToken != riderDevice {
				t.Errorf("device = %q, want the RIDER's device", sent[0].DeviceToken)
			}
			if sent[0].Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", sent[0].Title, tt.wantTitle)
			}
			copyText := sent[0].Title + " " + sent[0].Body
			for _, forbidden := range []string{"17:30", "5:30", "UTC", "Aug", "August", "2026"} {
				if strings.Contains(copyText, forbidden) {
					t.Errorf("copy %q contains %q — scheduled pushes must not render a time", copyText, forbidden)
				}
			}
		})
	}
}

func TestNotifierDueNotifiesRider(t *testing.T) {
	tests := []struct {
		name        string
		vehicleName string
		namerErr    error
		wantTitle   string
	}{
		{name: "names the car", vehicleName: "Blue Whale", wantTitle: "Blue Whale is heading your way"},
		{name: "unnamed car falls back", wantTitle: "Your car is heading your way"},
		{name: "lookup failure falls back", namerErr: errors.New("db down"), wantTitle: "Your car is heading your way"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{name: tt.vehicleName, err: tt.namerErr})

			n.handleDue(dueEvent())
			n.Wait()

			// MYR-535: ride.due wakes BOTH parties — the rider ("heading your
			// way") and the owner ("time to head out"). The rider's copy is
			// what this test pins; the owner's has its own test below.
			sent := sender.Sent()
			if len(sent) != 2 {
				t.Fatalf("sent %d notifications, want 2 (rider + owner)", len(sent))
			}
			var rider *Notification
			for i := range sent {
				if sent[i].DeviceToken == riderDevice {
					rider = &sent[i]
				}
			}
			if rider == nil {
				t.Fatal("no notification reached the RIDER's device")
			}
			if rider.Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", rider.Title, tt.wantTitle)
			}
		})
	}
}

// TestNotifierDueNotifiesOwner pins the MYR-535 owner half: at dispatch —
// which now fires EARLY enough for the car to make the pickup instant — the
// owner is told to head out, named with the rider's first name when the
// resolver has one and anonymously otherwise. Payload policy holds: no time,
// no place.
func TestNotifierDueNotifiesOwner(t *testing.T) {
	tests := []struct {
		name      string
		requester *fakeRequesterNamer
		wantTitle string
	}{
		{name: "names the rider", requester: &fakeRequesterNamer{name: "Sam"}, wantTitle: "Sam's pickup is coming up"},
		{name: "no usable name falls back", requester: &fakeRequesterNamer{}, wantTitle: "Your rider's pickup is coming up"},
		{name: "lookup failure falls back", requester: &fakeRequesterNamer{err: errors.New("db down")}, wantTitle: "Your rider's pickup is coming up"},
		{name: "unwired resolver falls back", requester: nil, wantTitle: "Your rider's pickup is coming up"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})
			if tt.requester != nil {
				n = n.WithRequesterNames(tt.requester)
			}

			n.handleDue(dueEvent())
			n.Wait()

			var owner *Notification
			sent := sender.Sent()
			for i := range sent {
				if sent[i].DeviceToken == ownerDevice {
					owner = &sent[i]
				}
			}
			if owner == nil {
				t.Fatal("no notification reached the OWNER's device")
			}
			if owner.Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", owner.Title, tt.wantTitle)
			}
			if owner.Body != "Your car has the route — time to head out." {
				t.Errorf("body = %q", owner.Body)
			}
		})
	}
}

// TestNotifierDueSelfRideSendsOnce: an owner riding their own car (MYR-325)
// is both parties, and two pushes to one phone announcing one fact is noise —
// the rider push alone survives, exactly as before MYR-535.
func TestNotifierDueSelfRideSendsOnce(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	evt := events.NewEvent(events.RideDueEvent{
		RideRequestID: testRideID,
		VehicleID:     testVehicleID,
		RiderID:       testRiderID,
		OwnerID:       testRiderID, // one account in both roles
		ScheduledFor:  time.Now(),
		DueAt:         time.Now(),
	})
	n.handleDue(evt)
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d notifications, want 1 — a self-ride gets the rider push alone", len(sent))
	}
	if sent[0].Title != "Blue Whale is heading your way" {
		t.Errorf("title = %q, want the rider copy", sent[0].Title)
	}
}

// TestNotifierNoP1Leakage is the payload-policy guard. A notification renders
// on a LOCKED screen, so it may carry a first name and a vehicle nickname and
// nothing else — no surname, no address, no coordinates, no phone number.
func TestNotifierNoP1Leakage(t *testing.T) {
	// Values that exist elsewhere on the ride and must never reach a payload.
	const (
		surname     = "Lovelace"
		address     = "1 Ferry Building, San Francisco"
		coordinate  = "37.7955"
		phoneNumber = "+14155550123"
	)

	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"}).
		WithRequesterNames(&fakeRequesterNamer{name: "Ada"})

	scheduled := time.Now().Add(time.Hour)
	n.handleCreated(createdEvent(strptr("Ada "+surname), nil))
	n.handleCreated(createdEvent(strptr("Ada "+surname), &scheduled))
	for _, status := range []string{"accepted", "declined", "arrived"} {
		n.handleStatusChanged(statusEvent(status))
	}
	n.handleDue(dueEvent())
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 7 {
		t.Fatalf("sent %d notifications, want 7", len(sent))
	}
	for _, s := range sent {
		payload := strings.Join([]string{s.Title, s.Body, s.RideID}, " | ")
		for _, forbidden := range []string{surname, address, coordinate, phoneNumber} {
			if strings.Contains(payload, forbidden) {
				t.Errorf("payload %q leaks P1 value %q", payload, forbidden)
			}
		}
		// The first name IS allowed — prove the guard above is not passing
		// trivially by asserting the created titles still carry it.
		if strings.HasSuffix(s.Title, "wants a ride") && !strings.Contains(s.Title, "Ada") {
			t.Errorf("title %q lost the permitted first name", s.Title)
		}
	}
}

// --- failure and disabled paths -------------------------------------------

// TestNotifierFireAndForgetUnderSenderFailure proves a failing APNs never
// escapes the notifier: no panic, no blocking, and the other devices still get
// their turn.
func TestNotifierFireAndForgetUnderSenderFailure(t *testing.T) {
	sender := NewFakeSender()
	sender.Err = errors.New("apns unreachable")

	devices := newFakeDeviceStore()
	devices.byUser[testRiderID] = []Device{{Token: "tok-a"}, {Token: "tok-b"}}
	n := NewNotifier(sender, devices, nil, nil, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())

	n.handleStatusChanged(statusEvent("accepted"))
	n.Wait()

	if got := len(sender.Sent()); got != 2 {
		t.Errorf("attempted %d sends, want 2 (a failure must not abort the fan-out)", got)
	}
	if got := devices.deletedTokens(); len(got) != 0 {
		t.Errorf("deleted %v, want none (a transient failure is not an unregistration)", got)
	}
}

// TestNotifierUnregisteredDeletesDevice pins the APNs feedback loop: a 410
// removes the row so the next ride does not retry a phone that no longer
// exists, while the healthy sibling device is left alone.
func TestNotifierUnregisteredDeletesDevice(t *testing.T) {
	sender := NewFakeSender()
	sender.ErrByToken["tok-dead"] = ErrUnregistered

	devices := newFakeDeviceStore()
	devices.byUser[testRiderID] = []Device{{Token: "tok-dead"}, {Token: "tok-live"}}
	n := NewNotifier(sender, devices, nil, nil, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())

	n.handleStatusChanged(statusEvent("arrived"))
	n.Wait()

	got := devices.deletedTokens()
	if len(got) != 1 || got[0] != "tok-dead" {
		t.Errorf("deleted = %v, want exactly [tok-dead]", got)
	}
}

// TestNotifierThrottledDoesNotDelete guards the other direction: a 429 is
// backpressure, not a verdict on the token.
func TestNotifierThrottledDoesNotDelete(t *testing.T) {
	sender := NewFakeSender()
	sender.Err = ErrThrottled
	devices := newFakeDeviceStore()
	devices.byUser[testRiderID] = []Device{{Token: "tok-busy"}}
	n := NewNotifier(sender, devices, nil, nil, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())

	n.handleDue(dueEvent())
	n.Wait()

	if got := devices.deletedTokens(); len(got) != 0 {
		t.Errorf("deleted = %v, want none for a 429", got)
	}
}

// TestNotifierSkipsWhenInactive covers the two states the service ships in
// before the APNs secrets land, plus the kill-switch.
func TestNotifierSkipsWhenInactive(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		keyless bool
	}{
		{name: "keyless", enabled: true, keyless: true},
		{name: "kill-switched", enabled: false},
		{name: "kill-switched and keyless", keyless: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := NewFakeSender()
			var s Sender = sender
			if tt.keyless {
				s = nil
			}
			devices := newFakeDeviceStore()
			devices.byUser[testOwnerID] = []Device{{Token: ownerDevice}}
			n := NewNotifier(s, devices, nil, nil, &fakeVehicleNamer{}, Config{Enabled: tt.enabled}, discardLogger())

			// Must not panic on a nil sender, and must not deliver.
			n.handleCreated(createdEvent(strptr("Ada"), nil))
			n.handleStatusChanged(statusEvent("accepted"))
			n.handleDue(dueEvent())
			n.Wait()

			if got := len(sender.Sent()); got != 0 {
				t.Errorf("sent %d notifications, want 0", got)
			}
		})
	}
}

func TestNotifierDeviceLookupFailureIsSwallowed(t *testing.T) {
	sender := NewFakeSender()
	devices := newFakeDeviceStore()
	devices.listErr = errors.New("db down")
	n := NewNotifier(sender, devices, nil, nil, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())

	n.handleDue(dueEvent())
	n.Wait()

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d notifications, want 0", got)
	}
}

func TestNotifierNoDevicesRegistered(t *testing.T) {
	sender := NewFakeSender()
	n := NewNotifier(sender, newFakeDeviceStore(), nil, nil, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())

	n.handleCreated(createdEvent(strptr("Ada"), nil))
	n.Wait()

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d notifications, want 0", got)
	}
}

// TestNotifierWrongPayloadType proves a mis-typed payload is ignored rather
// than panicking the bus goroutine.
func TestNotifierWrongPayloadType(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{})

	n.handleCreated(events.NewEvent(events.RideDueEvent{}))
	n.handleStatusChanged(events.NewEvent(events.RideRequestCreatedEvent{}))
	n.handleDue(events.NewEvent(events.RideStatusChangedEvent{}))
	n.Wait()

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d notifications, want 0", got)
	}
}

// --- bus integration -------------------------------------------------------

// TestNotifierSubscribeDeliversFromRealBus wires the notifier to an actual
// ChannelBus, so the three topic registrations are exercised end to end rather
// than assumed.
func TestNotifierSubscribeDeliversFromRealBus(t *testing.T) {
	logger := discardLogger()
	bus := events.NewChannelBus(events.BusConfig{}, events.NoopBusMetrics{}, logger)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})
	if err := n.Subscribe(bus); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(n.Unsubscribe)

	ctx := context.Background()
	for _, evt := range []events.Event{createdEvent(strptr("Ada"), nil), statusEvent("declined"), dueEvent()} {
		if err := bus.Publish(ctx, evt); err != nil {
			t.Fatalf("Publish(%s): %v", evt.Topic, err)
		}
	}

	// The bus delivers asynchronously; poll until all four land — ride.due
	// wakes BOTH parties since MYR-535.
	deadline := time.Now().Add(2 * time.Second)
	for len(sender.Sent()) < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 4 {
		t.Fatalf("received %d notifications, want 4 (one per topic, two for ride.due)", len(sent))
	}

	titles := map[string]bool{}
	for _, s := range sent {
		titles[s.Title] = true
	}
	for _, want := range []string{
		"Ada wants a ride",
		"Blue Whale can't take this ride",
		"Blue Whale is heading your way",
		"Your rider's pickup is coming up",
	} {
		if !titles[want] {
			t.Errorf("missing notification %q (got %v)", want, titles)
		}
	}
}

// TestNotifierWaitDrainsConcurrentSends pins MYR-410.
//
// Wait always runs on a different goroutine from the one starting workers — in
// production the bus's delivery goroutine, stood in for here, racing main's
// `defer notifier.Wait()` — and nothing orders the two. So a fan-out may be
// registered at the very moment a drain begins, and Wait must still cover it. A
// sync.WaitGroup did not: its counter may be read empty in that window, which
// is a deploy landing mid-ride and a rider never getting the alert their
// transition had already earned.
//
// This is the regression test #364 wrote for the Live Activity notifier,
// replayed here on the alert notifier — the copy of the bug that fix
// deliberately left behind.
func TestNotifierWaitDrainsConcurrentSends(t *testing.T) {
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	const sends = 200
	handled := make(chan struct{})
	go func() {
		defer close(handled)
		for i := 0; i < sends; i++ {
			n.handleCreated(createdEvent(nil, nil))
		}
	}()

	// Drain repeatedly against the live producer, the way shutdown races it.
	for draining := true; draining; {
		n.Wait()
		select {
		case <-handled:
			draining = false
		default:
		}
	}

	// Every fan-out is registered by now, so this drain is the whole set.
	n.Wait()
	if got := len(sender.Sent()); got != sends {
		t.Errorf("Wait() returned with %d of %d notifications sent — a drain that ends early drops a push the rider was owed", got, sends)
	}
}
