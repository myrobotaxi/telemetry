package arrivalflash

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/drain"
	"github.com/myrobotaxi/telemetry/internal/events"
)

// commandFlashLights is the Tesla command name, which is also its key in the
// internal/commands registry (SignerRequired: it goes through the signing
// proxy, exactly like the nav dispatcher's pushes).
const commandFlashLights = "flash_lights"

// VehicleResolver resolves a vehicle cuid to its Tesla VIN. Satisfied in cmd/
// by the same adapter over the vehicle repo the nav dispatcher uses.
type VehicleResolver interface {
	ResolveVIN(ctx context.Context, vehicleID string) (string, error)
}

// TokenSource resolves a vehicle owner's Tesla OAuth access token (refreshing
// an expired one). Satisfied in cmd/ by the same adapter over the shared
// TeslaTokenResolver the nav dispatcher uses.
type TokenSource interface {
	ResolveToken(ctx context.Context, userID string) (string, error)
}

// CommandExecutor executes one Tesla vehicle command. Satisfied by
// *commands.Executor; a fake substitutes it in tests.
type CommandExecutor interface {
	Execute(ctx context.Context, req commands.Request) (commands.Result, error)
}

// Flasher subscribes to `ride.waypoint_arrived` and flashes the car's
// headlights three times. Construct with New, then Start; Stop and Wait on
// shutdown. See the package doc for what it may conclude and why every failure
// is silent.
type Flasher struct {
	bus      events.Bus
	vehicles VehicleResolver
	tokens   TokenSource
	executor CommandExecutor
	cfg      Config
	logger   *slog.Logger

	// fired is the exactly-once latch, keyed (ride, waypoint).
	fired *latch
	// sleep paces the flashes. A field rather than a direct time.Sleep so
	// tests can run the sequence without spending its wall clock; see
	// setSleeper.
	sleep func(ctx context.Context, d time.Duration) error
	// workers counts in-flight greetings so shutdown can drain them. Not a
	// sync.WaitGroup — see internal/drain.
	workers drain.Group

	sub    events.Subscription
	subbed bool
	ctx    context.Context
	cancel context.CancelFunc
}

// New builds a Flasher over the three seams. cfg's zero-valued knobs are
// replaced with production defaults; logger may be nil (a discard logger is
// substituted).
func New(
	bus events.Bus,
	vehicles VehicleResolver,
	tokens TokenSource,
	executor CommandExecutor,
	cfg Config,
	logger *slog.Logger,
) *Flasher {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	cfg = cfg.withDefaults()
	return &Flasher{
		bus:      bus,
		vehicles: vehicles,
		tokens:   tokens,
		executor: executor,
		cfg:      cfg,
		logger:   logger,
		fired:    newLatch(cfg.LatchSize),
		sleep:    sleepCtx,
	}
}

// setSleeper overrides the inter-flash pacing. Test-only seam; unexported
// because no production caller should swap the clock out from under a feature
// whose whole visible property is its rhythm.
func (f *Flasher) setSleeper(fn func(ctx context.Context, d time.Duration) error) { f.sleep = fn }

// Start subscribes to TopicRideWaypointArrived, or does nothing at all when the
// kill-switch is off. The context governs the Tesla calls the handler makes, so
// cancelling it stops the Flasher from touching a car even before Stop
// unsubscribes it.
func (f *Flasher) Start(ctx context.Context) error {
	if !f.cfg.Enabled {
		f.logger.Info("arrival light flash disabled by ARRIVAL_FLASH_ENABLED; " +
			"an arrival moves the ride and notifies the rider, and the car does nothing")
		return nil
	}
	f.ctx, f.cancel = context.WithCancel(ctx)

	sub, err := f.bus.Subscribe(events.TopicRideWaypointArrived, f.handle)
	if err != nil {
		f.cancel()
		return fmt.Errorf("arrivalflash.Flasher.Start: %w", err)
	}
	f.sub, f.subbed = sub, true
	f.logger.Info("arrival light flash enabled",
		slog.Int("flashes", f.cfg.Flashes),
		slog.Duration("interval", f.cfg.Interval),
		slog.Duration("timeout", f.cfg.Timeout),
	)
	return nil
}

// Stop unsubscribes and cancels any greeting in flight. Safe to call on a
// Flasher that never started (including a disabled one).
func (f *Flasher) Stop() error {
	if f.cancel != nil {
		f.cancel()
	}
	if !f.subbed {
		return nil
	}
	f.subbed = false
	if err := f.bus.Unsubscribe(f.sub); err != nil {
		return fmt.Errorf("arrivalflash.Flasher.Stop: %w", err)
	}
	return nil
}

// Wait blocks until every greeting that has STARTED has finished. Call AFTER
// bus.Close on shutdown, never before — see internal/drain for why the pair is
// the guarantee and neither half is one alone. Tests use it as a quiesce point.
//
// Nothing in the caller is obliged to wait at all: an abandoned greeting costs
// a rider a courtesy they never knew was coming, which is the same cost as a
// failed one. It is offered so a deploy landing at the exact moment a car
// arrives does not cut the gesture in half — one flash, then a dead process,
// which is the one failure mode that looks like a fault rather than like
// nothing.
func (f *Flasher) Wait() { f.workers.Wait() }

// handle is the events.Handler. It type-asserts, takes the exactly-once latch
// ON THE BUS GOROUTINE (so two redeliveries cannot both pass it), then hands
// the greeting to a tracked worker and returns immediately — the bus's serial
// per-subscriber loop must never be held for the ~3 s the flashes take, let
// alone the Timeout a sleeping car can cost.
func (f *Flasher) handle(evt events.Event) {
	ev, ok := evt.Payload.(events.RideWaypointArrivedEvent)
	if !ok {
		f.logger.Error("arrival flash: unexpected payload type on ride.waypoint_arrived",
			slog.String("event_id", evt.ID),
		)
		return
	}
	if !f.fired.claim(latchKey(ev.RideRequestID, ev.Waypoint)) {
		// Redelivery. Not a warning: the detector is exactly-once, so this is
		// the belt-and-braces path doing its job, and it is only interesting
		// when somebody is already looking.
		f.logger.Debug("arrival flash already fired for this waypoint; ignoring redelivery",
			slog.String("ride_request_id", ev.RideRequestID),
			slog.String("waypoint", ev.Waypoint),
		)
		return
	}

	done := f.workers.Track()
	go func() {
		defer done()
		ctx, cancel := context.WithTimeout(f.ctx, f.cfg.Timeout)
		defer cancel()
		f.greet(ctx, ev)
	}()
}

// greet resolves the car and the owner's token, then flashes. Every exit is a
// log line and nothing else — see the package doc: there is no party for whom a
// missed flash is actionable, and the ride's own state was settled before this
// event existed.
func (f *Flasher) greet(ctx context.Context, ev events.RideWaypointArrivedEvent) {
	log := f.logger.With(
		slog.String("ride_request_id", ev.RideRequestID),
		slog.String("waypoint", ev.Waypoint),
	)

	vin, err := f.vehicles.ResolveVIN(ctx, ev.VehicleID)
	if err != nil {
		log.Warn("arrival flash skipped: could not resolve the vehicle's VIN",
			slog.String("vehicle_id", ev.VehicleID),
			slog.String("error", err.Error()),
		)
		return
	}
	log = log.With(slog.String("vin", redactVIN(vin)))

	token, err := f.tokens.ResolveToken(ctx, ev.OwnerID)
	if err != nil {
		// Expired / never-linked / transient all land here together on
		// purpose. The nav dispatcher distinguishes them because it PERSISTS an
		// outcome an owner can be shown; this one has nothing to persist and
		// nobody to show, so the error text in the log is the whole story.
		log.Warn("arrival flash skipped: could not resolve the owner's Tesla token",
			slog.String("error", err.Error()),
		)
		return
	}

	f.flash(ctx, log, vin, token)
}

// flash sends the command Flashes times, Interval apart, and stops at the first
// refusal.
//
// This loop IS the attempt budget: one Execute per flash, no retry, so a
// waypoint can never cost more than cfg.Flashes commands however badly it goes.
// Abandoning the remainder on a failure is deliberate — a refusal here
// (unpaired key, expired scope, a car that would not wake within the executor's
// own budget) is not a condition that clears in 1.5 s, and a partial greeting
// reads as a fault where silence reads as a car that simply did not flash.
func (f *Flasher) flash(ctx context.Context, log *slog.Logger, vin, token string) {
	req := commands.Request{
		VIN:         vin,
		Command:     commandFlashLights,
		AccessToken: token,
		Scopes:      commands.ParseScopes(token),
	}

	for i := 0; i < f.cfg.Flashes; i++ {
		if i > 0 {
			if err := f.sleep(ctx, f.cfg.Interval); err != nil {
				log.Warn("arrival flash cut short: the greeting's budget ran out between flashes",
					slog.Int("flashes_sent", i),
					slog.Int("flashes_wanted", f.cfg.Flashes),
				)
				return
			}
		}
		if _, err := f.executor.Execute(ctx, req); err != nil {
			log.Warn("arrival flash failed; abandoning the rest of the greeting",
				slog.Int("flash", i+1),
				slog.Int("flashes_wanted", f.cfg.Flashes),
				slog.String("error", err.Error()),
			)
			return
		}
	}
	log.Info("arrival flash sent", slog.Int("flashes", f.cfg.Flashes))
}

// latchKey is the exactly-once key. The waypoint is part of it, not just the
// ride: a ride has more than one leg endpoint, and a future drop-off arrival
// must be able to greet even though the pickup already did.
func latchKey(rideRequestID, waypoint string) string { return rideRequestID + "|" + waypoint }

// sleepCtx sleeps for d, returning early if ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("arrival flash interval: %w", err)
		}
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("arrival flash interval: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// redactVIN shows only the last 4 chars of a VIN for logs (empty stays empty).
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}

// discardWriter drops log output for the nil-logger default.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
