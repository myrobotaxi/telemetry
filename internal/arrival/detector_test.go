package arrival

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

const (
	testVIN    = "7SAXCDE2NTF000001" // synthetic — VINs are P1 (see internal/vin)
	testRideID = "cride_538"
)

// --- doubles -----------------------------------------------------------

type fakeStore struct {
	mu    sync.Mutex
	cands []Candidate
	err   error
	calls int
}

func (s *fakeStore) ListArrivalCandidates(context.Context, int) ([]Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	out := make([]Candidate, len(s.cands))
	copy(out, s.cands)
	return out, nil
}

func (s *fakeStore) set(cands []Candidate, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cands, s.err = cands, err
}

func (s *fakeStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type fakeWriter struct {
	mu   sync.Mutex
	ids  []string
	err  error
	when time.Time
}

func (w *fakeWriter) MarkArrived(_ context.Context, id string) (events.RideStatusChangedEvent, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ids = append(w.ids, id)
	if w.err != nil {
		return events.RideStatusChangedEvent{}, w.err
	}
	return events.RideStatusChangedEvent{
		RideRequestID: id,
		VehicleID:     "cveh_1",
		RiderID:       "cusr_rider",
		OwnerID:       "cusr_owner",
		Status:        "arrived",
		UpdatedAt:     w.when,
	}, nil
}

func (w *fakeWriter) writes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.ids...)
}

// recordingBus captures published events without a real fan-out. It keeps the
// handlers PER TOPIC: the detector subscribes to two seams (frames, and the
// trip-edit invalidation), and a double that kept only the last one would
// silently deliver telemetry to the wrong handler.
type recordingBus struct {
	mu        sync.Mutex
	published []events.Event
	handlers  map[events.Topic]events.Handler
}

// handler is the telemetry frame path — what feed drives.
func (b *recordingBus) handler(evt events.Event) {
	b.dispatch(events.TopicVehicleTelemetry, evt)
}

// editTrip drives the trip-edit seam, as the handler bus would.
func (b *recordingBus) editTrip() {
	b.dispatch(events.TopicRideTripChanged,
		events.NewEvent(events.RideTripChangedEvent{RideRequestID: testRideID}))
}

func (b *recordingBus) dispatch(topic events.Topic, evt events.Event) {
	b.mu.Lock()
	h := b.handlers[topic]
	b.mu.Unlock()
	if h != nil {
		h(evt)
	}
}

func (b *recordingBus) Publish(_ context.Context, evt events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, evt)
	return nil
}

func (b *recordingBus) Subscribe(topic events.Topic, h events.Handler) (events.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handlers == nil {
		b.handlers = make(map[events.Topic]events.Handler, 2)
	}
	b.handlers[topic] = h
	return events.Subscription{ID: "sub:" + string(topic), Topic: topic}, nil
}

func (b *recordingBus) Unsubscribe(events.Subscription) error { return nil }
func (b *recordingBus) Close(context.Context) error           { return nil }
func (b *recordingBus) topics() []events.Topic {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]events.Topic, 0, len(b.published))
	for _, e := range b.published {
		out = append(out, e.Topic)
	}
	return out
}

// --- harness -----------------------------------------------------------

type harness struct {
	det    *Detector
	bus    *recordingBus
	store  *fakeStore
	writer *fakeWriter
	clock  time.Time
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{
		bus:    &recordingBus{},
		store:  &fakeStore{cands: []Candidate{testCandidate()}},
		writer: &fakeWriter{},
		clock:  time.Date(2026, 8, 13, 17, 0, 0, 0, time.UTC),
	}
	h.det = NewDetector(h.bus, h.store, h.writer, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.det.setNow(func() time.Time { return h.clock })
	if err := h.det.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.det.Stop() })
	return h
}

func testCandidate() Candidate {
	return Candidate{
		RideRequestID:   testRideID,
		VehicleID:       "cveh_1",
		RiderID:         "cusr_rider",
		OwnerID:         "cusr_owner",
		VIN:             testVIN,
		Waypoint:        events.WaypointPickup,
		TargetLatitude:  pickupLat,
		TargetLongitude: pickupLng,
	}
}

// feed delivers one frame through the bus handler, exactly as the bus would.
func (h *harness) feed(vin string, sec int, meters, mph float64) {
	lat, lng := offsetNorth(meters)
	speed := mph
	h.bus.handler(events.NewEvent(events.VehicleTelemetryEvent{
		VIN:       vin,
		CreatedAt: h.clock.Add(time.Duration(sec) * time.Second),
		Fields: map[string]events.TelemetryValue{
			string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: lat, Longitude: lng}},
			string(telemetry.FieldSpeed):    {FloatVal: &speed},
		},
	}))
}

// --- tests -------------------------------------------------------------

// TestDetectorAdvancesOnceOnScriptedApproach is the happy path end to end: a
// car drives in, parks, and keeps streaming for another two minutes. Exactly
// ONE write and ONE pair of events come out of it.
func TestDetectorAdvancesOnceOnScriptedApproach(t *testing.T) {
	h := newHarness(t, DefaultConfig())

	for _, s := range []struct {
		sec           int
		meters, speed float64
	}{
		{0, 500, 32}, {10, 200, 24}, {20, 60, 11}, {25, 15, 0},
		{35, 15, 0}, {45, 15, 0}, {60, 15, 0}, {120, 15, 0}, {180, 15, 0},
	} {
		h.feed(testVIN, s.sec, s.meters, s.speed)
	}

	if got := h.writer.writes(); len(got) != 1 || got[0] != testRideID {
		t.Fatalf("writes = %v, want exactly one for %s", got, testRideID)
	}
	// The status event first (what every existing consumer reacts to), then the
	// waypoint seam.
	want := []events.Topic{events.TopicRideStatusChanged, events.TopicRideWaypointArrived}
	got := h.bus.topics()
	if len(got) != len(want) {
		t.Fatalf("published %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("published[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// TestDetectorPublishesThePickupHandlerShape pins the contract that makes this
// a second WRITER rather than a second feature: what goes on the bus is the
// same RideStatusChangedEvent the owner's tap produces, so the rider's push,
// Live Activity and WS frame fire identically.
func TestDetectorPublishesThePickupHandlerShape(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})
	h.writer.when = h.clock.Add(30 * time.Second)

	h.feed(testVIN, 0, 10, 0)
	h.feed(testVIN, 20, 10, 0)

	if len(h.bus.published) != 2 {
		t.Fatalf("published %d events, want 2", len(h.bus.published))
	}

	status, ok := h.bus.published[0].Payload.(events.RideStatusChangedEvent)
	if !ok {
		t.Fatalf("first payload is %T, want RideStatusChangedEvent", h.bus.published[0].Payload)
	}
	if status.Status != "arrived" {
		t.Errorf("status = %q, want arrived", status.Status)
	}
	if status.PreviousStatus != statusAccepted {
		t.Errorf("previousStatus = %q, want %q", status.PreviousStatus, statusAccepted)
	}
	if status.RideRequestID != testRideID || status.RiderID != "cusr_rider" || status.OwnerID != "cusr_owner" {
		t.Errorf("ids = %+v, want the updated row's", status)
	}
	if !status.UpdatedAt.Equal(h.writer.when) {
		t.Errorf("updatedAt = %v, want the updated row's %v", status.UpdatedAt, h.writer.when)
	}

	waypoint, ok := h.bus.published[1].Payload.(events.RideWaypointArrivedEvent)
	if !ok {
		t.Fatalf("second payload is %T, want RideWaypointArrivedEvent", h.bus.published[1].Payload)
	}
	if waypoint.Waypoint != events.WaypointPickup {
		t.Errorf("waypoint = %q, want %q", waypoint.Waypoint, events.WaypointPickup)
	}
	if waypoint.RideRequestID != testRideID || waypoint.VehicleID != "cveh_1" {
		t.Errorf("waypoint event = %+v, want the candidate's ids", waypoint)
	}
}

// TestDetectorOwnerTapWins is the race the design turns on: the owner's tap
// lands first, so the guarded write matches no row. The detector must no-op —
// no retry, no event, no log-level noise.
func TestDetectorOwnerTapWins(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})
	h.writer.err = ErrRefused

	// A full dwell, then two minutes of the car sitting there.
	h.feed(testVIN, 0, 10, 0)
	h.feed(testVIN, 20, 10, 0)
	h.feed(testVIN, 40, 10, 0)
	h.feed(testVIN, 100, 10, 0)
	h.feed(testVIN, 140, 10, 0)

	if got := h.writer.writes(); len(got) != 1 {
		t.Errorf("attempted %d writes, want exactly 1 — a refusal is final", len(got))
	}
	if len(h.bus.published) != 0 {
		t.Errorf("published %v, want nothing — this detector lost the race", h.bus.topics())
	}
}

// TestDetectorRetriesAfterTransportFailure is the other side of the same coin:
// a database error says nothing about the ride, so the latch is released and a
// later frame tries again.
func TestDetectorRetriesAfterTransportFailure(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})
	h.writer.err = errors.New("connection reset by peer")

	h.feed(testVIN, 0, 10, 0)
	h.feed(testVIN, 20, 10, 0)
	if got := h.writer.writes(); len(got) != 1 {
		t.Fatalf("writes after first attempt = %d, want 1", len(got))
	}

	h.writer.mu.Lock()
	h.writer.err = nil
	h.writer.mu.Unlock()

	h.feed(testVIN, 30, 10, 0)
	if got := h.writer.writes(); len(got) != 2 {
		t.Fatalf("writes after recovery = %d, want 2", len(got))
	}
	if len(h.bus.published) != 2 {
		t.Errorf("published %v, want the status + waypoint pair", h.bus.topics())
	}
}

// TestDetectorIgnoresUnrelatedFrames: the overwhelming majority of frames come
// from cars with no live ride, and they must cost nothing but a map lookup.
func TestDetectorIgnoresUnrelatedFrames(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})

	h.feed("7SAYGDEF9TF000002", 0, 10, 0)
	h.feed("7SAYGDEF9TF000002", 20, 10, 0)
	h.feed("", 30, 10, 0)

	if got := h.writer.writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

// TestDetectorMovingCarNeverFires — the false-positive case the client would
// actually notice: the car crawls past the pickup in traffic without ever
// stopping.
func TestDetectorMovingCarNeverFires(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	for sec := 0; sec <= 300; sec += 5 {
		h.feed(testVIN, sec, 20, 6)
	}
	if got := h.writer.writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none — the car never stopped", got)
	}
}

// TestDetectorCachesCandidates pins the cost model: one read per TTL, not one
// per frame.
func TestDetectorCachesCandidates(t *testing.T) {
	h := newHarness(t, Config{Dwell: time.Hour, CandidateTTL: 15 * time.Second})

	for sec := 0; sec < 10; sec++ {
		h.feed(testVIN, sec, 500, 30)
	}
	if got := h.store.callCount(); got != 1 {
		t.Fatalf("store reads = %d, want 1 for a burst inside one TTL", got)
	}

	h.clock = h.clock.Add(16 * time.Second)
	h.feed(testVIN, 20, 500, 30)
	if got := h.store.callCount(); got != 2 {
		t.Errorf("store reads = %d, want 2 once the TTL expired", got)
	}
}

// TestDetectorSuspendsWhenCandidatesUnreadable: past the staleness ceiling the
// snapshot is dropped and detection stops, rather than deciding arrivals from
// a picture of the fleet nobody can confirm.
func TestDetectorSuspendsWhenCandidatesUnreadable(t *testing.T) {
	ttl := 15 * time.Second
	h := newHarness(t, Config{Dwell: 20 * time.Second, CandidateTTL: ttl})

	// Warm the cache, then take the database away.
	h.feed(testVIN, 0, 500, 30)
	h.store.set(nil, errors.New("pool exhausted"))

	// Inside the ceiling the cached candidate still serves: a blip must not
	// make the fleet disappear mid-dwell.
	h.clock = h.clock.Add(ttl + time.Second)
	h.feed(testVIN, 10, 10, 0)
	h.feed(testVIN, 30, 10, 0)
	if got := h.writer.writes(); len(got) != 1 {
		t.Fatalf("writes inside the staleness ceiling = %d, want 1", len(got))
	}

	// Past it, nothing is served at all.
	h2 := newHarness(t, Config{Dwell: 20 * time.Second, CandidateTTL: ttl})
	h2.feed(testVIN, 0, 500, 30)
	h2.store.set(nil, errors.New("pool exhausted"))
	h2.clock = h2.clock.Add(staleSnapshotFactor*ttl + time.Second)
	h2.feed(testVIN, 10, 10, 0)
	h2.feed(testVIN, 30, 10, 0)
	if got := h2.writer.writes(); len(got) != 0 {
		t.Errorf("writes past the staleness ceiling = %d, want 0", len(got))
	}
}

// TestDetectorSkipsAmbiguousVehicle: one car with two live leg-1 rides cannot
// be resolved, so neither ride is advanced.
func TestDetectorSkipsAmbiguousVehicle(t *testing.T) {
	h := newHarness(t, Config{Dwell: 20 * time.Second})
	second := testCandidate()
	second.RideRequestID = "cride_other"
	h.store.set([]Candidate{testCandidate(), second}, nil)

	h.feed(testVIN, 0, 10, 0)
	h.feed(testVIN, 20, 10, 0)

	if got := h.writer.writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none — the detector cannot tell which ride arrived", got)
	}
}

// TestDetectorPrunesTracksOfDepartedRides keeps the in-memory latch map bounded
// by CONCURRENT live rides rather than by lifetime ride volume.
func TestDetectorPrunesTracksOfDepartedRides(t *testing.T) {
	ttl := 15 * time.Second
	h := newHarness(t, Config{Dwell: time.Hour, CandidateTTL: ttl})

	h.feed(testVIN, 0, 10, 0)
	if len(h.det.tracks) != 1 {
		t.Fatalf("tracks = %d, want 1 while the ride is live", len(h.det.tracks))
	}

	h.store.set(nil, nil) // the ride left the candidate set
	h.clock = h.clock.Add(ttl + time.Second)
	h.feed(testVIN, 20, 10, 0)

	if len(h.det.tracks) != 0 {
		t.Errorf("tracks = %d, want 0 once the ride left the set", len(h.det.tracks))
	}
}

// TestDetectorStopIsIdempotent — Stop runs from a defer in cmd/ and must be
// safe whatever state the detector is in.
func TestDetectorStopIsIdempotent(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	if err := h.det.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := h.det.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	never := NewDetector(&recordingBus{}, &fakeStore{}, &fakeWriter{}, DefaultConfig(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := never.Stop(); err != nil {
		t.Fatalf("Stop on a never-started detector: %v", err)
	}
}
