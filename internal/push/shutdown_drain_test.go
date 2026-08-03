package push

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/drain"
	"github.com/myrobotaxi/telemetry/internal/events"
)

// The shutdown sequence these tests pin is cmd/telemetry-server's (MYR-410):
// every self-unsubscribing consumer stops, THEN bus.Close, THEN the notifier
// drains. The pairing is the guarantee — neither half is one on its own, which
// is the thing the first version of this fix got wrong.

// shutdownDrainTimeout is the test's stand-in for main's busDrainTimeout. Long
// enough that a slow machine never trips it, short enough that a genuine hang
// fails the test rather than the whole suite.
const shutdownDrainTimeout = 10 * time.Second

// TestShutdownSequenceDeliversEventAcceptedJustBeforeExit is the integration
// test the suite lacked: an event the bus accepted immediately before shutdown
// must be delivered before the process would exit.
//
// It runs main's real sequence — publish, then bus.Close, then Wait — against a
// real ChannelBus and a real Notifier, and asserts on the state at the instant
// Wait returns, because that instant is when the process exits.
func TestShutdownSequenceDeliversEventAcceptedJustBeforeExit(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{}, events.NoopBusMetrics{}, discardLogger())
	sender := NewFakeSender()
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})
	if err := n.Subscribe(bus); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Publish hard and immediately shut down: the last events are still in the
	// subscriber's buffer, unhandled, when the sequence begins. That is the
	// production case — SIGTERM lands while a rider's transition is in flight.
	const published = 50
	for i := 0; i < published; i++ {
		if err := bus.Publish(context.Background(), createdEvent(nil, nil)); err != nil {
			t.Fatalf("Publish(%d): %v", i, err)
		}
	}

	// main's sequence. The context is FRESH on purpose — main's ctx is already
	// cancelled at this point, and Close derives its drain deadline from what
	// it is handed.
	closeCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()
	if err := bus.Close(closeCtx); err != nil {
		t.Fatalf("bus.Close: %v", err)
	}
	n.Wait()

	if got := len(sender.Sent()); got != published {
		t.Errorf("after the full shutdown sequence, %d of %d notifications were sent — an accepted event was dropped at exit", got, published)
	}
}

// blockingSender holds every Send until released, so a drain can be observed
// giving up on work that is genuinely still in flight.
type blockingSender struct{ release chan struct{} }

func (b *blockingSender) Send(ctx context.Context, _ Notification) error {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

// TestNotifierWaitContextReportsAbandonedFanouts pins the bounded shutdown
// drain (MYR-410).
//
// A fan-out runs on a fresh Background context bounded only by cfg.Timeout,
// which SIGTERM cannot shorten, and bus.Close hands this drain the whole
// buffered backlog at once. Unbounded, shutdown outlives the platform's kill
// timeout and is SIGKILLed mid-push. Bounded, it loses the same pushes but says
// how many — which is the difference between a dropped notification you can
// find in the logs and one you cannot.
func TestNotifierWaitContextReportsAbandonedFanouts(t *testing.T) {
	sender := &blockingSender{release: make(chan struct{})}
	defer close(sender.release)
	n := newTestNotifier(t, sender, &fakeVehicleNamer{name: "Blue Whale"})

	const stuck = 3
	for i := 0; i < stuck; i++ {
		n.handleCreated(createdEvent(nil, nil))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	inFlight, err := n.WaitContext(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if inFlight != stuck {
		t.Errorf("in-flight = %d, want %d — an abandoned drain must report what it dropped", inFlight, stuck)
	}
	if elapsed > 5*time.Second {
		t.Errorf("WaitContext took %v — it waited on cfg.Timeout instead of its deadline", elapsed)
	}
}

// TestNotifierWaitAloneMissesBufferedEvents is the deterministic proof that the
// drain is NOT the guarantee on its own — the correction MYR-410's first pass
// needed.
//
// Subscriber channels are buffered (1000 in prod), so an event Publish has
// accepted can sit in the buffer with its handler never entered. Nothing has
// been counted yet, so a drain reads zero and returns — byte-for-byte the
// sync.WaitGroup outcome the counter was supposed to fix. Only bus.Close makes
// that accepted-but-unhandled work STARTED, and only then can a drain see it.
//
// The handler here models the notifier's exact shape (count on the delivery
// goroutine, work on a detached worker) and is gated, so the buffered events
// are buffered by construction rather than by luck.
func TestNotifierWaitAloneMissesBufferedEvents(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{}, events.NoopBusMetrics{}, discardLogger())

	var workers drain.Group
	var handled atomic.Int64
	entered := make(chan struct{}, 1)
	release := make(chan struct{})

	first := true
	handler := func(events.Event) {
		// Park the delivery goroutine on the FIRST event, BEFORE it counts
		// anything — the position a handler is genuinely in when SIGTERM
		// arrives mid-delivery. Everything published behind it lands in the
		// buffer, unhandled.
		if first {
			first = false
			entered <- struct{}{}
			<-release
		}
		done := workers.Track()
		go func() {
			defer done()
			handled.Add(1)
		}()
	}

	if _, err := bus.Subscribe(events.TopicRideRequestCreated, handler); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := bus.Publish(context.Background(), createdEvent(nil, nil)); err != nil {
		t.Fatalf("Publish(blocker): %v", err)
	}
	<-entered // the delivery goroutine is now parked inside the handler

	const buffered = 3
	for i := 0; i < buffered; i++ {
		if err := bus.Publish(context.Background(), createdEvent(nil, nil)); err != nil {
			t.Fatalf("Publish(%d): %v", i, err)
		}
	}

	// Shutdown the WRONG way round: drain without closing the bus first. This
	// is what main did before MYR-410's second pass, and what the drain's own
	// docs wrongly claimed was sufficient.
	workers.Wait()

	if got := handled.Load(); got != 0 {
		t.Fatalf("test is not pinning what it claims: %d events were handled before the drain", got)
	}
	// Wait returned having covered NOTHING, with four events the bus had
	// already accepted still undelivered. At shutdown there is no next drain —
	// those four are the dropped pushes, exactly as under a sync.WaitGroup.

	// Now the right way round: Close first, so every buffered event is run
	// through the handler and becomes work the counter can see, and only then
	// drain.
	close(release)
	closeCtx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()
	if err := bus.Close(closeCtx); err != nil {
		t.Fatalf("bus.Close: %v", err)
	}
	workers.Wait()

	if got := handled.Load(); got != buffered+1 {
		t.Errorf("after bus.Close + drain, %d of %d events were handled — Close is what runs the buffered backlog through the handler", got, buffered+1)
	}
}
