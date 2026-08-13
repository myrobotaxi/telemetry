// Package integration wires real components onto a real event bus and asserts
// that a seam between two of them actually closes.
//
// MYR-563 exists because every link of the stop-advance chain was tested and
// the CHAIN was not. internal/arrival proved the detector publishes
// ride.waypoint_arrived; internal/dispatch proved that event advances the leg
// and re-shares the next target; internal/store proved the candidate read names
// the current stop. All three passed for the whole ten minutes a real car sat
// at a real stop with nothing happening, because the thing that was broken —
// what a PARKED car's only available frame can prove — lived in the gap between
// the frame source and the first of them.
//
// So this test starts where the evidence starts: a telemetry frame of the exact
// shape Tesla produces for a stationary vehicle, published on a real bus, with
// the detector and the dispatcher both subscribed to it as the server wires
// them. Nothing is stubbed between the frame and the re-share but the database
// and the car.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/arrival"
	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/dispatch"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

const (
	e2eVIN     = "7SAXCDE2NTF000563" // synthetic — VINs are P1
	e2eRideID  = "cride563"
	e2eVehicle = "cveh563"
	e2eStopID  = "crs821"
)

// The stop's pin, and the spot in its car park where the car actually came to
// rest — 18 m away, which is an ordinary distance between a door pin and a bay.
var (
	stopLat, stopLng   = 33.0762, -96.8100
	parkedLat          = stopLat + 18.0/111_320.0
	parkedLng          = stopLng
	dropoffPlace       = events.RidePlace{Latitude: 33.0300, Longitude: -96.8300, Label: "Nordstrom"}
	frameInterval      = 25 * time.Second // the MYR-394 REST poll cadence
	dwellForTest       = 20 * time.Second
	candidateTTLForTst = time.Hour // one read; the set does not change mid-test
)

// --- seam doubles: only the database and the car -------------------------

type e2eCandidateStore struct {
	mu    sync.Mutex
	cands []arrival.Candidate
}

func (s *e2eCandidateStore) ListArrivalCandidates(context.Context, int) ([]arrival.Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]arrival.Candidate(nil), s.cands...), nil
}

// e2eWriter is the pickup's status write. This ride is enroute, so it must
// never be called — a stop does not touch the lifecycle.
type e2eWriter struct{ calls int }

func (w *e2eWriter) MarkArrived(context.Context, string) (events.RideStatusChangedEvent, error) {
	w.calls++
	return events.RideStatusChangedEvent{}, arrival.ErrRefused
}

// e2eStopStore is the guarded AdvanceStopArrival: the arrived stop completes,
// and with no stop left the final drop-off becomes the next target. Refuses
// every call after the first, exactly as the real statement does.
type e2eStopStore struct {
	mu    sync.Mutex
	calls []string
}

func (s *e2eStopStore) AdvanceStopArrival(_ context.Context, rideID, stopID string) (dispatch.StopAdvance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, rideID+"/"+stopID)
	if len(s.calls) > 1 {
		return dispatch.StopAdvance{}, dispatch.ErrStopNotAdvanced
	}
	return dispatch.StopAdvance{NextStopID: "", NextTarget: dropoffPlace}, nil
}

func (s *e2eStopStore) advances() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

type e2eVehicles struct{}

func (e2eVehicles) ResolveVIN(context.Context, string) (string, error) { return e2eVIN, nil }

type e2eTokens struct{}

func (e2eTokens) ResolveToken(context.Context, string) (string, error) { return "tok", nil }

// e2eExecutor is the car. It records the share requests the dispatcher sends.
type e2eExecutor struct {
	mu    sync.Mutex
	calls []commands.Request
}

func (e *e2eExecutor) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, req)
	return commands.Result{}, nil
}

func (e *e2eExecutor) requests() []commands.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]commands.Request(nil), e.calls...)
}

// e2eOutcomes accepts every claim; leg-1 is already long since resolved on this
// ride, and the stop advance's re-share takes the edited-leg path anyway.
type e2eOutcomes struct{}

func (e2eOutcomes) ClaimDispatch(context.Context, string) (bool, error) { return true, nil }
func (e2eOutcomes) RecordDispatchOutcome(context.Context, string, dispatch.Outcome, *string) error {
	return nil
}
func (e2eOutcomes) ClaimDropoffDispatch(context.Context, string) (bool, error) { return true, nil }
func (e2eOutcomes) RecordDropoffDispatchOutcome(context.Context, string, dispatch.Outcome, *string) error {
	return nil
}

// --- the test ------------------------------------------------------------

// parkedFrame is the MYR-394 REST backfill frame for a stationary vehicle: a
// location, and nothing else. Tesla answers speed=null and shift_state=null for
// a parked car and addDriveStateFields skips both rather than inventing a zero
// or a "P", so this is the complete field set — and, because the stream stops
// when the car parks, the only one available while it sits there.
func parkedFrame(at time.Time) events.Event {
	return events.NewEvent(events.VehicleTelemetryEvent{
		VIN:       e2eVIN,
		CreatedAt: at,
		Fields: map[string]events.TelemetryValue{
			string(telemetry.FieldLocation): {
				LocationVal: &events.Location{Latitude: parkedLat, Longitude: parkedLng},
			},
		},
	})
}

// TestParkedCarAtAMidLegStopAdvancesTheLegAndResharesTheDropoff is the MYR-563
// prod sequence, end to end: the ride is enroute with the drop-off leg already
// shared, a stop was added mid-leg and is 'current', and the car is parked in
// its car park sending nothing but REST position fixes.
//
// It asserts the three links in order — the observation, the guarded write, and
// the re-share — because the field failure was that the FIRST one never
// happened and the other two were consequently never reached.
func TestParkedCarAtAMidLegStopAdvancesTheLegAndResharesTheDropoff(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{}, events.NoopBusMetrics{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	arrived := make(chan events.RideWaypointArrivedEvent, 4)
	if _, err := bus.Subscribe(events.TopicRideWaypointArrived, func(e events.Event) {
		if ev, ok := e.Payload.(events.RideWaypointArrivedEvent); ok {
			arrived <- ev
		}
	}); err != nil {
		t.Fatalf("subscribe waypoint: %v", err)
	}

	// The dispatcher, with the leg advance wired, subscribed to the same bus
	// the detector will publish on.
	stops := &e2eStopStore{}
	exec := &e2eExecutor{}
	disp := dispatch.New(
		e2eVehicles{}, e2eTokens{}, exec, e2eOutcomes{},
		dispatch.Config{Enabled: true, MaxRetries: 0, Backoff: time.Millisecond},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	).WithStopAdvance(stops)
	if _, err := disp.Subscribe(bus); err != nil {
		t.Fatalf("dispatcher subscribe: %v", err)
	}

	// The detector, watching the ride's CURRENT stop — the one added mid-leg.
	writer := &e2eWriter{}
	det := arrival.NewDetector(bus, &e2eCandidateStore{cands: []arrival.Candidate{{
		RideRequestID:   e2eRideID,
		VehicleID:       e2eVehicle,
		RiderID:         "crider563",
		OwnerID:         "cowner563",
		VIN:             e2eVIN,
		Waypoint:        events.WaypointStop(e2eStopID),
		TargetLatitude:  stopLat,
		TargetLongitude: stopLng,
	}}}, writer, arrival.Config{
		Dwell:        dwellForTest,
		CandidateTTL: candidateTTLForTst,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := det.Start(context.Background()); err != nil {
		t.Fatalf("detector start: %v", err)
	}
	t.Cleanup(func() { _ = det.Stop() })

	// Four REST poll cycles at the same spot — 100 seconds of a car that is
	// demonstrably not going anywhere.
	base := time.Date(2026, 8, 13, 23, 30, 0, 0, time.UTC)
	for i := range 4 {
		if err := bus.Publish(context.Background(), parkedFrame(base.Add(time.Duration(i)*frameInterval))); err != nil {
			t.Fatalf("publish frame %d: %v", i, err)
		}
	}

	// LINK 1 — the observation.
	var ev events.RideWaypointArrivedEvent
	select {
	case ev = <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("no ride.waypoint_arrived from a car parked at the stop for 100s (MYR-563)")
	}
	if want := events.WaypointStop(e2eStopID); ev.Waypoint != want {
		t.Fatalf("waypoint = %q, want %q", ev.Waypoint, want)
	}
	if ev.RideRequestID != e2eRideID || ev.VehicleID != e2eVehicle {
		t.Errorf("event ids = %+v, want the ride's", ev)
	}
	if writer.calls != 0 {
		t.Errorf("MarkArrived called %d times, want 0 — a stop is not a pickup", writer.calls)
	}

	// LINK 2 — the guarded write, exactly once however many frames followed.
	waitFor(t, func() bool { return len(stops.advances()) == 1 })
	if got := stops.advances(); got[0] != e2eRideID+"/"+e2eStopID {
		t.Errorf("advance = %q, want %q", got[0], e2eRideID+"/"+e2eStopID)
	}

	// LINK 3 — the re-share of the next target, through the sequencer.
	waitFor(t, func() bool { return len(exec.requests()) >= 1 })
	reqs := exec.requests()
	if len(reqs) != 1 {
		t.Fatalf("executor calls = %d, want exactly 1 re-share", len(reqs))
	}
	if got, want := sharedValue(reqs[0]), placeValue(dropoffPlace); got != want {
		t.Errorf("re-share destination = %q, want the final drop-off %q", got, want)
	}
}

// sharedValue returns the nav destination text a share request carries. The
// dispatcher sends the coordinate pair as the navigation_request `value`, so
// that string IS the assertion that the car was pointed at the right place.
func sharedValue(req commands.Request) string {
	v, _ := req.Params["value"].(string)
	return v
}

// placeValue renders a place the way coordShareValue does, so the expectation
// is written in the same terms the dispatcher produces.
func placeValue(place events.RidePlace) string {
	return fmt.Sprintf("%v,%v", place.Latitude, place.Longitude)
}

// waitFor polls cond until it holds or the deadline passes. The dispatcher does
// its work on its own goroutine, so the assertions after a publish are
// eventual — but bounded, and a failure here is a real one rather than a flake.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
