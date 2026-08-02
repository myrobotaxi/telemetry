package push

import (
	"context"
	"errors"
	"sort"
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
	// read (MYR-172). Values carry the ride's STATUS as well as its id, because
	// that is what decides how the teardown ends each card (MYR-421).
	ridesByVehicle map[string][]VehicleRide

	ended   map[string]int
	deleted []string
	swept   time.Duration
	// pushed records every MarkPushed key in call order, which is what the
	// anti-starvation test asserts the rotation against.
	pushed []ActivityKey
	// progress records every SaveProgress call in order, keyed by (ride, user),
	// so the leg-progress tests can assert what was PERSISTED as well as what
	// was sent — the two are the same promise seen from either end (MYR-398).
	progress    []savedProgress
	progressErr error
	// alerts records every SaveAlertedPhase call in order, so the island-expand
	// tests can assert what was PERSISTED as well as what was sent (MYR-398).
	alerts   []savedAlert
	alertErr error

	// completedAt is `go_ride_requests.completed_at` for the rides this fixture
	// has completed (MYR-421). RidesAwaitingEnd reads it against clock the way
	// the real query reads the column against NOW().
	completedAt map[string]time.Time
	// heldFor records the hold the ticker asked for, so a test can assert the
	// horizon is DismissAfter without reaching into the ticker.
	heldFor    time.Duration
	heldEndErr error
	// clock is the fake's own NOW, moved by a test to walk the hold window.
	// Separate from the notifier's clock only so a case can advance the
	// database's idea of time without also moving the push timestamps.
	clock func() time.Time

	contextErr error
	listErr    error
	vehicleErr error
	markErr    error
}

func newFakeActivityStore() *fakeActivityStore {
	return &fakeActivityStore{
		byRide:         map[string][]Activity{},
		context:        map[string]RideContext{},
		ridesByVehicle: map[string][]VehicleRide{},
		ended:          map[string]int{},
		completedAt:    map[string]time.Time{},
		clock:          func() time.Time { return fixedNow },
	}
}

// completeRide marks a ride terminal at `at`, which is what the status write
// stamps on go_ride_requests.completed_at.
func (f *fakeActivityStore) completeRide(rideID string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rc := f.context[rideID]
	rc.Status = statusCompleted
	f.context[rideID] = rc
	f.completedAt[rideID] = at
}

// advanceClock moves the store's NOW, so the held-end predicate can be walked
// across the hold horizon without sleeping.
func (f *fakeActivityStore) advanceClock(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := f.clock()
	f.clock = func() time.Time { return base.Add(d) }
}

// RidesAwaitingEnd mimics queryListRidesAwaitingActivityEnd: completed rides
// whose completion instant is at least heldFor old and that still have a LIVE
// row, oldest completion first.
//
// The live-row half is modelled by byRide rather than asserted separately,
// because that is exactly the coupling under test — a ride tombstoned during
// the window (the rider swiped the card away, or the app ended it locally) must
// drop out of this list rather than be pushed an end nobody is watching for.
func (f *fakeActivityStore) RidesAwaitingEnd(_ context.Context, heldFor time.Duration, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.heldEndErr != nil {
		return nil, f.heldEndErr
	}
	f.heldFor = heldFor

	cutoff := f.clock().Add(-heldFor)
	type due struct {
		ride string
		at   time.Time
	}
	var rows []due
	for ride, at := range f.completedAt {
		if at.After(cutoff) || len(f.byRide[ride]) == 0 {
			continue
		}
		rows = append(rows, due{ride: ride, at: at})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].at.Equal(rows[j].at) {
			return rows[i].ride < rows[j].ride
		}
		return rows[i].at.Before(rows[j].at)
	})

	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(out) == limit {
			break
		}
		out = append(out, r.ride)
	}
	return out, nil
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

// savedProgress is one SaveProgress call.
type savedProgress struct {
	key    ActivityKey
	anchor ProgressAnchor
}

func (f *fakeActivityStore) SaveProgress(_ context.Context, key ActivityKey, anchor ProgressAnchor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.progressErr != nil {
		return f.progressErr
	}
	f.progress = append(f.progress, savedProgress{key: key, anchor: anchor})
	// Mimic the real row: the anchor the next push reads is the one this write
	// stored, on both the lifecycle and the ticker read paths.
	for i := range f.byRide[key.RideRequestID] {
		if f.byRide[key.RideRequestID][i].UserID == key.UserID {
			f.byRide[key.RideRequestID][i].Progress = anchor
		}
	}
	for i := range f.legs {
		if f.legs[i].RideRequestID == key.RideRequestID && f.legs[i].UserID == key.UserID {
			f.legs[i].Progress = anchor
		}
	}
	return nil
}

// savedAlert is one SaveAlertedPhase call.
type savedAlert struct {
	key   ActivityKey
	phase AlertPhase
}

// SaveAlertedPhase mimics the real guarded UPDATE, including the guard: a write
// that would LOWER the mark is a silent no-op. Reproducing the refusal here and
// not merely the write is what lets a test drive two passes over one leg and
// see the second one decline to alert, which is the whole once-per-ride
// promise (MYR-398).
func (f *fakeActivityStore) SaveAlertedPhase(_ context.Context, key ActivityKey, phase AlertPhase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alertErr != nil {
		return f.alertErr
	}
	f.alerts = append(f.alerts, savedAlert{key: key, phase: phase})

	for i := range f.byRide[key.RideRequestID] {
		if f.byRide[key.RideRequestID][i].UserID == key.UserID &&
			f.byRide[key.RideRequestID][i].AlertedPhase < phase {
			f.byRide[key.RideRequestID][i].AlertedPhase = phase
		}
	}
	for i := range f.legs {
		if f.legs[i].RideRequestID == key.RideRequestID && f.legs[i].UserID == key.UserID &&
			f.legs[i].AlertedPhase < phase {
			f.legs[i].AlertedPhase = phase
		}
	}
	return nil
}

// savedAlertsFor returns the phases persisted for one (ride, user), in order.
//
// rideID is the same fixture in every current caller, which unparam objects to.
// It is taken anyway because the twin savedProgressFor next door takes the pair,
// and a helper that filtered on only half the natural key would quietly pass the
// day a second ride is added.
//
//nolint:unparam // deliberate — see the note above
func (f *fakeActivityStore) savedAlertsFor(rideID, userID string) []AlertPhase {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []AlertPhase
	for _, a := range f.alerts {
		if a.key.RideRequestID == rideID && a.key.UserID == userID {
			out = append(out, a.phase)
		}
	}
	return out
}

// savedProgressFor returns the anchors persisted for one (ride, user), in order.
func (f *fakeActivityStore) savedProgressFor(rideID, userID string) []ProgressAnchor {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ProgressAnchor
	for _, p := range f.progress {
		if p.key.RideRequestID == rideID && p.key.UserID == userID {
			out = append(out, p.anchor)
		}
	}
	return out
}

func (f *fakeActivityStore) RideIDsWithActivitiesForVehicle(_ context.Context, vehicleID string) ([]VehicleRide, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vehicleErr != nil {
		return nil, f.vehicleErr
	}
	return append([]VehicleRide(nil), f.ridesByVehicle[vehicleID]...), nil
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
//
// The wanted lingers are LITERAL DURATIONS, not the constants the code reads.
// Written as DismissAfter / DismissPromptly this table would assert only that
// the map is wired to some constant, and every value in it could be changed
// without a single test failing — which is exactly how MYR-406's five minutes
// could drift back to fifteen, or how folding the two constants into one would
// move the unhappy endings by accident. Each number here is a product decision
// (MYR-406 for completed, MYR-194 for the rest) and changing one must mean
// changing this line and saying why.
//
// The linger is measured from the END's own instant, which is the pair's second
// push on a completed ride (MYR-418) and the only push on the others — hence
// `wantPushes` and the offset applied below. Measuring it from the alerting
// update instead would make the number the rider experiences depend on which
// half of the pair the test happened to look at.
//
// On a completed ride that second push is now five minutes late (MYR-421), so
// the case runs the ticker's held-end pass to reach it. The number asserted is
// unchanged and deliberately so: the linger is a property of the END, and
// holding the end moves WHEN the card starts its five minutes, never how long
// they are.
func TestActivityNotifierTerminalDismissal(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		wantPushes int
		held       bool
		wantLinger time.Duration
	}{
		{"completed lingers five minutes, matching the client's own linger", "completed", 2, true, 5 * time.Minute},
		{"declined dismisses promptly", "declined", 1, false, 30 * time.Second},
		{"cancelled dismisses promptly", "cancelled", 1, false, 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, sender, store := newTestActivityNotifier(t, nil)
			store.context[activityRideID] = RideContext{Status: tt.status, VehicleName: "Blue Whale", Destination: "Home"}
			if tt.held {
				store.completeRide(activityRideID, fixedNow)
			}

			n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
				RideRequestID: activityRideID,
				RiderID:       testRiderID,
				Status:        tt.status,
			}))
			n.Wait()
			if tt.held {
				holdExpires(t, n, store)
			}

			sent := sender.Sent()
			if len(sent) != tt.wantPushes {
				t.Fatalf("sent %d updates, want %d", len(sent), tt.wantPushes)
			}
			end := sent[len(sent)-1]
			if end.Event != ActivityEventEnd {
				t.Fatalf("event = %q, want end for terminal status %q", end.Event, tt.status)
			}
			if end.DismissalDate == nil {
				t.Fatal("terminal send carries no dismissal-date")
			}
			// The end's own timestamp, which trails the alerting update by
			// endAfterAlertGap on a completed ride and is `now` on the others.
			wantAt := end.Timestamp.Add(tt.wantLinger)
			if got := *end.DismissalDate; !got.Equal(wantAt) {
				t.Errorf("dismissal-date = %s, want %s (linger %s past the end's own timestamp)",
					got, wantAt, tt.wantLinger)
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
	// Literal, for the reason given on TestActivityNotifierTerminalDismissal:
	// MYR-406 moved `completed` only, and this ending must not follow it.
	if sent[0].DismissalDate == nil || !sent[0].DismissalDate.Equal(fixedNow.Add(30*time.Second)) {
		t.Error("reservation expiry did not dismiss promptly (30s)")
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

// TestActivityNotifierWaitDrainsConcurrentSends pins MYR-398.
//
// Wait always runs on a different goroutine from the one starting workers — in
// production the bus's delivery goroutine, stood in for here — and nothing
// orders the two. So a send may be registered at the very moment a drain
// begins, and Wait must still cover it. A sync.WaitGroup did not: its counter
// may be read empty in that window, which is a shutdown returning before the
// last push, and -race caught it on main.
func TestActivityNotifierWaitDrainsConcurrentSends(t *testing.T) {
	n, sender, _ := newTestActivityNotifier(t, nil)

	const sends = 200
	handled := make(chan struct{})
	go func() {
		defer close(handled)
		for i := 0; i < sends; i++ {
			n.handleStatusChanged(events.NewEvent(events.RideStatusChangedEvent{
				RideRequestID: activityRideID,
				RiderID:       testRiderID,
				Status:        "accepted",
			}))
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

	// Every worker is registered by now, so this drain is the whole set.
	n.Wait()
	if got := len(sender.Sent()); got != sends {
		t.Errorf("Wait() returned with %d of %d updates sent — a drain that ends early drops the rider's last state", got, sends)
	}
}
