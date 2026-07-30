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

	return NewNotifier(sender, devices, nil, namer, Config{Enabled: true}, discardLogger())
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

// TestNotifierStatusChangedNotifiesRider covers which transitions speak, which
// stay silent, and the copy for each.
func TestNotifierStatusChangedNotifiesRider(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		vehicleName string
		wantSent    bool
		wantTitle   string
	}{
		{name: "accepted", status: "accepted", vehicleName: "Blue Whale", wantSent: true, wantTitle: "Your ride is confirmed"},
		{name: "declined names the car", status: "declined", vehicleName: "Blue Whale", wantSent: true, wantTitle: "Blue Whale can't take this ride"},
		{name: "declined falls back", status: "declined", wantSent: true, wantTitle: "Your car can't take this ride"},
		{name: "arrived", status: "arrived", wantSent: true, wantTitle: "Your car is here — your turn to start"},
		{name: "requested is silent", status: "requested"},
		{name: "enroute is silent", status: "enroute"},
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
			if !tt.wantSent {
				if len(sent) != 0 {
					t.Fatalf("sent %d notifications for %q, want silence", len(sent), tt.status)
				}
				return
			}
			if len(sent) != 1 {
				t.Fatalf("sent %d notifications, want 1", len(sent))
			}
			if sent[0].DeviceToken != riderDevice {
				t.Errorf("device = %q, want the RIDER's device", sent[0].DeviceToken)
			}
			if !sent[0].Sandbox {
				t.Error("sandbox = false, want the device row's flag to reach the sender")
			}
			if sent[0].Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", sent[0].Title, tt.wantTitle)
			}
		})
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
		})
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
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	scheduled := time.Now().Add(time.Hour)
	n.handleCreated(createdEvent(strptr("Ada "+surname), nil))
	n.handleCreated(createdEvent(strptr("Ada "+surname), &scheduled))
	for _, status := range []string{"accepted", "declined", "arrived"} {
		n.handleStatusChanged(statusEvent(status))
	}
	n.handleDue(dueEvent())
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 6 {
		t.Fatalf("sent %d notifications, want 6", len(sent))
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
	n := NewNotifier(sender, devices, nil, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())

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
	n := NewNotifier(sender, devices, nil, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())

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
	n := NewNotifier(sender, devices, nil, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())

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
			n := NewNotifier(s, devices, nil, &fakeVehicleNamer{}, Config{Enabled: tt.enabled}, discardLogger())

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
	n := NewNotifier(sender, devices, nil, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())

	n.handleDue(dueEvent())
	n.Wait()

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d notifications, want 0", got)
	}
}

func TestNotifierNoDevicesRegistered(t *testing.T) {
	sender := NewFakeSender()
	n := NewNotifier(sender, newFakeDeviceStore(), nil, &fakeVehicleNamer{}, Config{Enabled: true}, discardLogger())

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

	// The bus delivers asynchronously; poll until all three land.
	deadline := time.Now().Add(2 * time.Second)
	for len(sender.Sent()) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 3 {
		t.Fatalf("received %d notifications, want 3 (one per topic)", len(sent))
	}

	titles := map[string]bool{}
	for _, s := range sent {
		titles[s.Title] = true
	}
	for _, want := range []string{
		"Ada wants a ride",
		"Blue Whale can't take this ride",
		"Blue Whale is heading your way",
	} {
		if !titles[want] {
			t.Errorf("missing notification %q (got %v)", want, titles)
		}
	}
}
