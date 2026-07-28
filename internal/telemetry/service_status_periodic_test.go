package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeInServiceLister scripts the in-service VIN list and records the limit it
// was asked for, so a test can prove the pass is bounded.
type fakeInServiceLister struct {
	mu     sync.Mutex
	calls  int
	limits []int
	vins   []string
	err    error
}

func (f *fakeInServiceLister) ListInServiceVINs(_ context.Context, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]string, len(f.vins))
	copy(out, f.vins)
	return out, nil
}

func (f *fakeInServiceLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeInServiceLister) lastLimit() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.limits) == 0 {
		return 0
	}
	return f.limits[len(f.limits)-1]
}

// TestRunInServicePassRunsTheEdgeBundle is the heart of MYR-320's structural
// fix: for each listed in-service VIN the pass must run the SAME read bundle a
// connectivity edge runs — the authoritative vehicle read, and (because an
// in-service car is by definition not streaming) the vehicle_data backfill.
//
// Reusing resolveViaRead rather than reimplementing the bundle is the whole
// design: it is why the debounce, the MYR-300 gate and the never-wake guarantee
// come along for free instead of being three more things to get right twice.
func TestRunInServicePassRunsTheEdgeBundle(t *testing.T) {
	t.Parallel()

	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "offline", InService: true},
		data: &VehicleData{
			VehicleConfig: &VehicleDataVehicleConfig{PerformancePackage: ptr("Performance")},
		},
	}
	lister := &fakeInServiceLister{vins: []string{svcTestVIN}}
	updater := &fakeStatusUpdater{}

	m := newTestMonitor(reader, updater,
		WithPeriodicInServicePoll(lister, PeriodicPollConfig{Enabled: true}))

	m.RunInServicePass(context.Background())

	if got := lister.callCount(); got != 1 {
		t.Errorf("list calls = %d, want 1", got)
	}
	if got := lister.lastLimit(); got != defaultRepollMaxPerPass {
		t.Errorf("list limit = %d, want the configured cap %d", got, defaultRepollMaxPerPass)
	}
	if got := reader.callCount(); got != 1 {
		t.Errorf("vehicle reads = %d, want 1 (the authoritative in_service read)", got)
	}
	if got := reader.dataCallCount(); got != 1 {
		t.Errorf("vehicle_data reads = %d, want 1 (the backfill for a non-streaming car)", got)
	}
	if got := updater.written(); len(got) != 1 || got[0] != serviceStatusInService {
		t.Errorf("statuses = %v, want [%s]", got, serviceStatusInService)
	}
}

// A pass must touch ONLY the vehicles the lister returned. The list IS the
// in_service filter — the pass has no business reading a car the store did not
// name, and an empty list must cost nothing at all.
func TestRunInServicePassTouchesOnlyListedVehicles(t *testing.T) {
	t.Parallel()

	t.Run("empty list does no Tesla work", func(t *testing.T) {
		t.Parallel()
		reader := &fakeVehicleReader{state: FleetVehicleState{State: "offline", InService: true}}
		lister := &fakeInServiceLister{vins: nil}
		m := newTestMonitor(reader, &fakeStatusUpdater{},
			WithPeriodicInServicePoll(lister, PeriodicPollConfig{Enabled: true}))

		m.RunInServicePass(context.Background())

		if got := reader.callCount(); got != 0 {
			t.Errorf("vehicle reads = %d, want 0 for an empty in-service list", got)
		}
	})

	t.Run("every listed vehicle is read exactly once", func(t *testing.T) {
		t.Parallel()
		reader := &fakeVehicleReader{state: FleetVehicleState{State: "offline", InService: true}}
		lister := &fakeInServiceLister{vins: []string{
			"7SAYGDET7TA613795", "5YJ3E1EA1NF000001", "5YJ3E1EA1NF000002",
		}}
		m := newTestMonitor(reader, &fakeStatusUpdater{},
			WithPeriodicInServicePoll(lister, PeriodicPollConfig{Enabled: true}))

		m.RunInServicePass(context.Background())

		if got := reader.callCount(); got != 3 {
			t.Errorf("vehicle reads = %d, want 3 (one per listed VIN)", got)
		}
	})

	t.Run("a failed list skips the pass without reading anything", func(t *testing.T) {
		t.Parallel()
		reader := &fakeVehicleReader{state: FleetVehicleState{State: "offline", InService: true}}
		lister := &fakeInServiceLister{err: errors.New("connection reset")}
		m := newTestMonitor(reader, &fakeStatusUpdater{},
			WithPeriodicInServicePoll(lister, PeriodicPollConfig{Enabled: true}))

		m.RunInServicePass(context.Background())

		if got := reader.callCount(); got != 0 {
			t.Errorf("vehicle reads = %d, want 0 — a failed list is a skipped pass, not a blind one", got)
		}
	})
}

// TestRunInServicePassRespectsEdgeDebounce is the no-double-fetch guarantee. A
// connectivity edge and a periodic pass share ONE per-VIN cooldown, so a car the
// edge just read is skipped by the pass — the periodic trigger must not double
// the Fleet API traffic for an actively-flapping car.
func TestRunInServicePassRespectsEdgeDebounce(t *testing.T) {
	t.Parallel()

	now := time.Now()
	clock := func() time.Time { return now }

	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "offline", InService: true},
		data:  &VehicleData{},
	}
	lister := &fakeInServiceLister{vins: []string{svcTestVIN}}
	m := newTestMonitor(reader, &fakeStatusUpdater{},
		withServiceClock(clock),
		WithPeriodicInServicePoll(lister, PeriodicPollConfig{Enabled: true}))

	// The connectivity edge reads first and stamps the cooldown.
	m.handleConnectivity(context.Background(), connectEvt())
	if got := reader.callCount(); got != 1 {
		t.Fatalf("after the edge, vehicle reads = %d, want 1", got)
	}

	// A pass INSIDE the cooldown must not read again.
	m.RunInServicePass(context.Background())
	if got := reader.callCount(); got != 1 {
		t.Errorf("vehicle reads = %d, want still 1 — the pass must share the edge's per-VIN debounce", got)
	}
	// The list still happened; only the per-VIN read was debounced. That is the
	// right shape: the cap is on Tesla traffic, not on knowing who is in service.
	if got := lister.callCount(); got != 1 {
		t.Errorf("list calls = %d, want 1", got)
	}

	// Once the cooldown lapses, the pass reads normally again.
	now = now.Add(defaultServiceReadCooldown + time.Second)
	m.RunInServicePass(context.Background())
	if got := reader.callCount(); got != 2 {
		t.Errorf("vehicle reads = %d, want 2 once the cooldown lapsed", got)
	}
}

// TestRunPeriodicInServicePollKillSwitch: false must mean NO work whatsoever,
// and it must return rather than idle — an operator who turns this off to stop
// Fleet API traffic has to be able to trust that it stopped.
func TestRunPeriodicInServicePollKillSwitch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		enabled bool
		unwired bool
	}{
		{name: "kill-switch off", enabled: false},
		{name: "lister not wired", enabled: true, unwired: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := &fakeVehicleReader{state: FleetVehicleState{State: "offline", InService: true}}
			lister := &fakeInServiceLister{vins: []string{svcTestVIN}}

			opts := []ServiceStatusMonitorOption{}
			if tt.unwired {
				opts = append(opts, WithPeriodicInServicePoll(nil, PeriodicPollConfig{Enabled: tt.enabled}))
			} else {
				opts = append(opts, WithPeriodicInServicePoll(lister, PeriodicPollConfig{
					Enabled:      tt.enabled,
					Interval:     time.Millisecond,
					StartupDelay: time.Millisecond,
				}))
			}
			m := newTestMonitor(reader, &fakeStatusUpdater{}, opts...)

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()

			done := make(chan struct{})
			go func() {
				m.RunPeriodicInServicePoll(ctx)
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("RunPeriodicInServicePoll did not return immediately when disabled")
			}
			if got := lister.callCount(); got != 0 {
				t.Errorf("list calls = %d, want 0 when disabled", got)
			}
			if got := reader.callCount(); got != 0 {
				t.Errorf("vehicle reads = %d, want 0 when disabled", got)
			}
		})
	}
}

// TestRunPeriodicInServicePollStartupPass proves a deploy does not wait a full
// interval: the first pass fires after StartupDelay, and the loop keeps going.
// Half the reason this feature exists is that a car sitting in service produces
// no edge, so a restart that waited 15 minutes to notice would be a regression
// of its own.
func TestRunPeriodicInServicePollStartupPass(t *testing.T) {
	t.Parallel()

	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "offline", InService: true},
		data:  &VehicleData{},
	}
	lister := &fakeInServiceLister{vins: []string{svcTestVIN}}
	m := newTestMonitor(reader, &fakeStatusUpdater{},
		WithPeriodicInServicePoll(lister, PeriodicPollConfig{
			Enabled: true,
			// Tiny but non-zero: withDefaults substitutes the production values
			// for anything non-positive, so a zero here would make the test wait
			// 30 seconds for the startup pass alone.
			Interval:       20 * time.Millisecond,
			StartupDelay:   time.Millisecond,
			JitterFraction: 0.01,
		}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		m.RunPeriodicInServicePoll(ctx)
		close(done)
	}()

	// The startup pass alone must land well before one interval's worth of
	// ticks could account for it.
	waitFor(t, 2*time.Second, func() bool { return lister.callCount() >= 1 })

	// And the loop must keep running afterwards, not fire once and stop.
	waitFor(t, 2*time.Second, func() bool { return lister.callCount() >= 3 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPeriodicInServicePoll did not stop on context cancellation")
	}
}

// TestPeriodicPollConfigDefaults: a zero or negative knob must fall back to the
// production default rather than producing a hot loop or an unbounded query.
func TestPeriodicPollConfigDefaults(t *testing.T) {
	t.Parallel()

	got := PeriodicPollConfig{Enabled: true}.withDefaults()

	if got.Interval != defaultRepollInterval {
		t.Errorf("Interval = %v, want %v", got.Interval, defaultRepollInterval)
	}
	if got.StartupDelay != defaultRepollStartupDelay {
		t.Errorf("StartupDelay = %v, want %v", got.StartupDelay, defaultRepollStartupDelay)
	}
	if got.ListTimeout != defaultRepollListTimeout {
		t.Errorf("ListTimeout = %v, want %v", got.ListTimeout, defaultRepollListTimeout)
	}
	if got.MaxPerPass != defaultRepollMaxPerPass {
		t.Errorf("MaxPerPass = %d, want %d", got.MaxPerPass, defaultRepollMaxPerPass)
	}
	if got.JitterFraction != defaultRepollJitter {
		t.Errorf("JitterFraction = %v, want %v", got.JitterFraction, defaultRepollJitter)
	}
	// A negative interval is a mistake, not an instruction — it must not
	// survive into a timer.
	neg := PeriodicPollConfig{Interval: -time.Hour}.withDefaults()
	if neg.Interval != defaultRepollInterval {
		t.Errorf("negative Interval = %v, want the default %v", neg.Interval, defaultRepollInterval)
	}
}

// TestJitterDuration: the spread must stay inside the requested band (so the
// cadence still means what the config says), and every degenerate input must
// yield something a timer can accept.
func TestJitterDuration(t *testing.T) {
	t.Parallel()

	base := 15 * time.Minute
	fraction := 0.10

	minWant := time.Duration(float64(base) * (1 - fraction))
	maxWant := time.Duration(float64(base) * (1 + fraction))

	sawDistinct := false
	first := jitterDuration(base, fraction)
	for i := 0; i < 200; i++ {
		got := jitterDuration(base, fraction)
		if got < minWant || got > maxWant {
			t.Fatalf("jitterDuration = %v, want within [%v, %v]", got, minWant, maxWant)
		}
		if got != first {
			sawDistinct = true
		}
	}
	if !sawDistinct {
		t.Error("jitterDuration returned a constant — replicas would stay synchronised, " +
			"which is the entire point of jittering")
	}

	if got := jitterDuration(base, 0); got != base {
		t.Errorf("jitterDuration(_, 0) = %v, want the unjittered %v", got, base)
	}
	if got := jitterDuration(0, fraction); got != 0 {
		t.Errorf("jitterDuration(0, _) = %v, want 0", got)
	}
	// A fraction of 1 could otherwise produce a non-positive duration, which
	// time.NewTimer fires immediately on — a hot loop.
	for i := 0; i < 200; i++ {
		if got := jitterDuration(base, 1); got <= 0 {
			t.Fatalf("jitterDuration(_, 1) = %v, want a positive duration", got)
		}
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}
