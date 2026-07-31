package push

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// fakeActivityStore is the in-memory ActivityStore + ActivityLegStore double.
//
// Mutex-guarded because the notifier's work runs on bounded workers and the
// tombstone/delete paths run on their own detached contexts, so a single event
// produces concurrent reads and writes here.
type fakeActivityStore struct {
	mu sync.Mutex

	byRide  map[string][]Activity
	context map[string]RideContext
	legs    []ActivityLeg

	// ridesByVehicle backs RideIDsWithActivitiesForVehicle — the owner-teardown
	// read (MYR-172).
	ridesByVehicle map[string][]string

	ended   map[string]int
	deleted []string
	swept   time.Duration
	// pushed records every MarkPushed key in call order, which is what the
	// anti-starvation test asserts the rotation against.
	pushed []ActivityKey

	contextErr error
	listErr    error
	vehicleErr error
	markErr    error
}

func newFakeActivityStore() *fakeActivityStore {
	return &fakeActivityStore{
		byRide:         map[string][]Activity{},
		context:        map[string]RideContext{},
		ridesByVehicle: map[string][]string{},
		ended:          map[string]int{},
	}
}

func (f *fakeActivityStore) ActivitiesForRide(_ context.Context, rideID string) ([]Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Activity(nil), f.byRide[rideID]...), nil
}

func (f *fakeActivityStore) EndActivitiesForRide(_ context.Context, rideID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := int64(len(f.byRide[rideID]))
	f.ended[rideID]++
	delete(f.byRide, rideID)
	return n, nil
}

func (f *fakeActivityStore) DeleteActivityToken(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, token)
	return nil
}

func (f *fakeActivityStore) RideContextFor(_ context.Context, rideID string) (RideContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.contextErr != nil {
		return RideContext{}, f.contextErr
	}
	rc, ok := f.context[rideID]
	if !ok {
		return RideContext{}, errors.New("ride not found")
	}
	return rc, nil
}

func (f *fakeActivityStore) ActiveLegs(_ context.Context, limit int) ([]ActivityLeg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.legs) > limit {
		return append([]ActivityLeg(nil), f.legs[:limit]...), nil
	}
	return append([]ActivityLeg(nil), f.legs...), nil
}

func (f *fakeActivityStore) RideIDsWithActivitiesForVehicle(_ context.Context, vehicleID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vehicleErr != nil {
		return nil, f.vehicleErr
	}
	return append([]string(nil), f.ridesByVehicle[vehicleID]...), nil
}

// MarkPushed mimics the real UPDATE: it stamps the delivered rows and REORDERS
// the leg list so they sort last. Without the reorder the double would make the
// starvation bug untestable — the fake would rotate for free.
func (f *fakeActivityStore) MarkPushed(_ context.Context, keys []ActivityKey) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return 0, f.markErr
	}
	f.pushed = append(f.pushed, keys...)

	stamped := make(map[ActivityKey]bool, len(keys))
	for _, k := range keys {
		stamped[k] = true
	}
	var kept, moved []ActivityLeg
	for _, leg := range f.legs {
		if stamped[ActivityKey{RideRequestID: leg.RideRequestID, UserID: leg.UserID}] {
			moved = append(moved, leg)
			continue
		}
		kept = append(kept, leg)
	}
	f.legs = append(kept, moved...)
	return int64(len(keys)), nil
}

func (f *fakeActivityStore) pushedKeys() []ActivityKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ActivityKey(nil), f.pushed...)
}

func (f *fakeActivityStore) SweepStale(_ context.Context, olderThan time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swept = olderThan
	return 0, nil
}

// endCount reports how many times the fixture ride was tombstoned.
func (f *fakeActivityStore) endCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended[activityRideID]
}

func (f *fakeActivityStore) deletedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

const (
	activityRideID = "ride_live_1"
	riderToken     = "rider-activity-token"
)

// newTestActivityNotifier wires a notifier over one rider Activity on one ride.
func newTestActivityNotifier(t *testing.T, prefs PrefStore) (*ActivityNotifier, *FakeActivitySender, *fakeActivityStore) {
	t.Helper()
	store := newFakeActivityStore()
	store.byRide[activityRideID] = []Activity{{
		RideRequestID: activityRideID,
		UserID:        testRiderID,
		Token:         riderToken,
	}}
	store.context[activityRideID] = RideContext{
		Status:      "accepted",
		VehicleName: "Blue Whale",
		Destination: "Home",
		ETAMinutes:  intPtr(6),
	}

	sender := NewFakeActivitySender()
	n := NewActivityNotifier(sender, store, prefs, Config{Enabled: true}, discardLogger())
	n.now = func() time.Time { return fixedNow }
	return n, sender, store
}

func intPtr(v int) *int { return &v }

// TestActivityNotifierUpdatesOnStatusChange is the ordinary path.
func TestActivityNotifierUpdatesOnStatusChange(t *testing.T) {
	n, sender, _ := newTestActivityNotifier(t, nil)

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID,
		RiderID:       testRiderID,
		Status:        "accepted",
	}))
	n.Wait()

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d updates, want 1", len(sent))
	}
	if sent[0].Event != ActivityEventUpdate {
		t.Errorf("event = %q, want update", sent[0].Event)
	}
	if sent[0].LowPriority {
		t.Error("a lifecycle transition sent at conserving priority; MYR-194 gives lifecycle events precedence over ETA ticks")
	}
	if sent[0].DismissalDate != nil {
		t.Error("dismissal-date set on a non-terminal transition")
	}
	if got, want := sent[0].ContentState.Status, "accepted"; got != want {
		t.Errorf("content-state status = %q, want %q", got, want)
	}
	// The ETA is converted from the car's carried DURATION into an INSTANT.
	if sent[0].ContentState.ETA == nil {
		t.Fatal("content-state carries no ETA despite a known nav ETA")
	}
	if got, want := *sent[0].ContentState.ETA, fixedNow.Add(6*time.Minute).Unix(); got != want {
		t.Errorf("content-state eta = %d, want %d", got, want)
	}
}

// TestActivityNotifierTerminalDismissal covers MYR-194 decision 5 across every
// terminal status — including the linger asymmetry, which is the whole point:
// completed gets a long look, the unhappy endings go promptly but not instantly.
func TestActivityNotifierTerminalDismissal(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		wantLinger time.Duration
	}{
		{"completed lingers so the rider sees the arrival", "completed", DismissAfter},
		{"declined dismisses promptly", "declined", DismissPromptly},
		{"cancelled dismisses promptly", "cancelled", DismissPromptly},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, sender, store := newTestActivityNotifier(t, nil)
			store.context[activityRideID] = RideContext{Status: tt.status, VehicleName: "Blue Whale", Destination: "Home"}

			n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
				RideRequestID: activityRideID,
				RiderID:       testRiderID,
				Status:        tt.status,
			}))
			n.Wait()

			sent := sender.Sent()
			if len(sent) != 1 {
				t.Fatalf("sent %d updates, want 1", len(sent))
			}
			if sent[0].Event != ActivityEventEnd {
				t.Fatalf("event = %q, want end for terminal status %q", sent[0].Event, tt.status)
			}
			if sent[0].DismissalDate == nil {
				t.Fatal("terminal send carries no dismissal-date")
			}
			if got, want := *sent[0].DismissalDate, fixedNow.Add(tt.wantLinger); !got.Equal(want) {
				t.Errorf("dismissal-date = %s, want %s (linger %s)", got, want, tt.wantLinger)
			}
			if store.endCount() != 1 {
				t.Error("terminal send did not tombstone the registry rows")
			}
		})
	}
}

// TestActivityNotifierSendsBeforeTombstoning pins the ordering.
//
// A row ended first would be excluded from its own final push, and the rider
// would be left looking at whatever state happened to arrive last — for a
// declined ride, "your car is on its way", forever.
func TestActivityNotifierSendsBeforeTombstoning(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	store.context[activityRideID] = RideContext{Status: "declined"}

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID,
		RiderID:       testRiderID,
		Status:        "declined",
	}))
	n.Wait()

	if len(sender.Sent()) != 1 {
		t.Fatalf("the final update was not sent (%d sends); the rows were ended first", len(sender.Sent()))
	}
	if store.endCount() != 1 {
		t.Error("rows were not tombstoned after the final send")
	}
}

// TestActivityNotifierNonTerminalStatusesNeverEnd guards the statuses most
// easily mistaken for endings. `arrived` is the car REACHING the pickup and
// `enroute` is leg two — ending on either would kill the Activity at the
// busiest moment of the ride.
func TestActivityNotifierNonTerminalStatusesNeverEnd(t *testing.T) {
	for _, status := range []string{"requested", "accepted", "arrived", "enroute"} {
		t.Run(status, func(t *testing.T) {
			n, sender, store := newTestActivityNotifier(t, nil)
			store.context[activityRideID] = RideContext{Status: status}

			n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
				RideRequestID: activityRideID,
				RiderID:       testRiderID,
				Status:        status,
			}))
			n.Wait()

			sent := sender.Sent()
			if len(sent) != 1 {
				t.Fatalf("sent %d updates, want 1", len(sent))
			}
			if sent[0].Event != ActivityEventUpdate {
				t.Errorf("status %q produced event %q, want update", status, sent[0].Event)
			}
			if store.endCount() != 0 {
				t.Errorf("status %q tombstoned the registry", status)
			}
		})
	}
}

// TestActivityNotifierMutedRiderGetsNothing is the MYR-349 / MYR-194 decision 7
// gate. The Activity still RUNS on the phone — the app started it locally, and
// starting one needs no permission — it simply stops being told anything.
func TestActivityNotifierMutedRiderGetsNothing(t *testing.T) {
	prefs := newFakePrefStore()
	muted := DefaultPrefs()
	muted.RideLifecycle = false
	prefs.byUser[testRiderID] = muted

	n, sender, _ := newTestActivityNotifier(t, prefs)

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID,
		RiderID:       testRiderID,
		Status:        "accepted",
	}))
	n.Wait()

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d updates to a rider who muted ride updates, want 0", got)
	}
	if prefs.lookups() == 0 {
		t.Error("the preference gate never ran")
	}
}

// TestActivityNotifierPrefLookupFailureSendsAnyway pins the fail-open direction,
// matching the alert notifier's twin. A notifier that silenced itself because a
// preference read failed would leave riders watching a frozen lock screen with
// nothing anywhere saying why.
func TestActivityNotifierPrefLookupFailureSendsAnyway(t *testing.T) {
	prefs := newFakePrefStore()
	prefs.err = errors.New("prefs unavailable")

	n, sender, _ := newTestActivityNotifier(t, prefs)

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID,
		RiderID:       testRiderID,
		Status:        "accepted",
	}))
	n.Wait()

	if got := len(sender.Sent()); got != 1 {
		t.Errorf("sent %d updates when the preference store failed, want 1 (fail open)", got)
	}
}

// TestActivityNotifierUnregisteredDropsOnlyTheActivity pins the blast radius of
// an APNs 410 on this surface: the ACTIVITY is gone, not the phone. Deleting
// the device row here would silently disable the rider's ordinary ride alerts
// because they dismissed a Live Activity.
func TestActivityNotifierUnregisteredDropsOnlyTheActivity(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)
	sender.ErrByToken = map[string]error{riderToken: ErrUnregistered}

	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID,
		RiderID:       testRiderID,
		Status:        "accepted",
	}))
	n.Wait()

	deleted := store.deletedTokens()
	if len(deleted) != 1 || deleted[0] != riderToken {
		t.Errorf("deleted tokens = %v, want exactly the rejected activity token", deleted)
	}
}

// TestActivityNotifierReservationExpiryEnds covers the one lifecycle ending the
// event bus cannot see: the sweeper resolves a late reservation into the
// dispatch columns and leaves the ride at `accepted`, so without this seam a
// rider whose car never came would watch an Activity promise a pickup forever.
func TestActivityNotifierReservationExpiryEnds(t *testing.T) {
	n, sender, store := newTestActivityNotifier(t, nil)

	n.EndForReservationExpiry(context.Background(), activityRideID)

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d updates, want 1", len(sent))
	}
	if sent[0].Event != ActivityEventEnd {
		t.Errorf("event = %q, want end", sent[0].Event)
	}
	// The caller's status wins over the row's, which still reads `accepted`.
	if got, want := sent[0].ContentState.Status, "reservation_expired"; got != want {
		t.Errorf("content-state status = %q, want %q — the row still reads accepted, but the lock screen must show the ending", got, want)
	}
	if sent[0].DismissalDate == nil || !sent[0].DismissalDate.Equal(fixedNow.Add(DismissPromptly)) {
		t.Error("reservation expiry did not dismiss promptly")
	}
	if store.endCount() != 1 {
		t.Error("reservation expiry did not tombstone the registry rows")
	}
}

// TestActivityNotifierKeylessSendsNothing pins the pre-secrets mode.
func TestActivityNotifierKeylessSendsNothing(t *testing.T) {
	store := newFakeActivityStore()
	store.byRide[activityRideID] = []Activity{{RideRequestID: activityRideID, UserID: testRiderID, Token: riderToken}}
	store.context[activityRideID] = RideContext{Status: "accepted"}

	n := NewActivityNotifier(nil, store, nil, Config{Enabled: true}, discardLogger())
	n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID,
		RiderID:       testRiderID,
		Status:        "completed",
	}))
	n.Wait()

	if store.endCount() != 0 {
		t.Error("a keyless notifier tombstoned rows it never pushed to")
	}
}

// TestActivityNotifierSubscribesToStatusTopic proves the wiring end to end
// through a real bus, rather than only calling the handler directly.
func TestActivityNotifierSubscribesToStatusTopic(t *testing.T) {
	n, sender, _ := newTestActivityNotifier(t, nil)

	bus := events.NewChannelBus(events.BusConfig{}, events.NoopBusMetrics{}, discardLogger())
	if err := n.Subscribe(bus); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	t.Cleanup(n.Unsubscribe)

	if err := bus.Publish(context.Background(), events.NewEvent(events.RideStatusChangedEvent{
		RideRequestID: activityRideID,
		RiderID:       testRiderID,
		Status:        "accepted",
	})); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	// The bus delivers asynchronously; poll until the send lands.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n.Wait()
		if len(sender.Sent()) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no Live Activity update delivered through the bus")
}
