package arrival

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// ErrRefused is what the writer seam returns when the guarded write
// matched no row: the ride was picked up by its owner first, was cancelled,
// advanced, or vanished. It is an ORDINARY outcome — the detector no-ops
// silently on it, because every one of those means somebody else already
// decided what this ride's status is.
var ErrRefused = errors.New("arrival: guarded write refused")

// Writer is the detector's write seam, satisfied by the ride-request
// repo through a cmd/ adapter.
//
// The implementation MUST be the same dormancy-guarded transition the owner's
// "Picked up" tap uses (store.UpdateStatusFromDispatched, allowed-from
// `accepted`, target `arrived`) — not a second write with the same effect.
// That is the whole race story: the owner tapping at the same instant is
// arbitrated by one UPDATE in the database, first writer wins, and this
// detector losing costs nothing.
//
// It returns the RideStatusChangedEvent built from the UPDATED row, exactly as
// the pickup handler's mutateStatusWith builds it, so the rider's push, Live
// Activity and WS frame cannot tell the two writers apart.
type Writer interface {
	MarkArrived(ctx context.Context, rideRequestID string) (events.RideStatusChangedEvent, error)
}

// statusAccepted is the status every arrival transitions OUT of, carried onto
// the published event as PreviousStatus. Unlike the HTTP handler's — which is
// whatever its pre-check read saw — this one is exact: the guarded write's
// allowed-from set is the single value `accepted`, so a write that succeeded
// proves the row held it.
const statusAccepted = "accepted"

// Detector watches decoded telemetry frames and advances a ride to `arrived`
// when its car is observed parked at the pickup. See the package doc for the
// rule and for why it is tuned to fire late rather than early.
type Detector struct {
	bus        events.Bus
	cfg        Config
	candidates *candidateCache
	writer     Writer
	logger     *slog.Logger

	// tracks holds per-ride dwell state, keyed by ride-request id. A plain map
	// with no mutex: the bus delivers serially per subscription, so this and
	// the cache are only ever touched from the handler goroutine.
	tracks map[string]*track

	// now is the clock used for candidate-cache ageing ONLY. The dwell is
	// measured off frame timestamps (see Fix.At), never off this.
	now func() time.Time

	sub    events.Subscription
	subbed bool
	ctx    context.Context
	cancel context.CancelFunc
}

// NewDetector builds a Detector over the two seams. cfg's zero-valued knobs are
// replaced with production defaults; cfg.Enabled is NOT consulted here — cmd/
// decides whether to construct and start one at all.
func NewDetector(bus events.Bus, store CandidateStore, writer Writer, cfg Config, logger *slog.Logger) *Detector {
	cfg = cfg.withDefaults()
	return &Detector{
		bus:        bus,
		cfg:        cfg,
		candidates: &candidateCache{store: store, cfg: cfg, logger: logger},
		writer:     writer,
		logger:     logger,
		tracks:     make(map[string]*track),
		now:        time.Now,
	}
}

// setNow overrides the cache clock. Test-only seam; unexported because no
// production caller should swap it.
func (d *Detector) setNow(fn func() time.Time) { d.now = fn }

// Start subscribes to TopicVehicleTelemetry. The context governs the candidate
// reads and arrival writes the handler makes, so cancelling it stops the
// detector from touching the database even before Stop unsubscribes it.
func (d *Detector) Start(ctx context.Context) error {
	d.ctx, d.cancel = context.WithCancel(ctx)

	sub, err := d.bus.Subscribe(events.TopicVehicleTelemetry, d.handleFrame)
	if err != nil {
		d.cancel()
		return fmt.Errorf("arrival.Detector.Start: %w", err)
	}
	d.sub, d.subbed = sub, true
	d.logger.Info("auto-arrival detector started",
		slog.Float64("radius_meters", d.cfg.RadiusMeters),
		slog.Duration("dwell", d.cfg.Dwell),
		slog.Duration("candidate_ttl", d.cfg.CandidateTTL),
	)
	return nil
}

// Stop unsubscribes and cancels in-flight store work. Safe to call on a
// detector that never started.
func (d *Detector) Stop() error {
	if d.cancel != nil {
		d.cancel()
	}
	if !d.subbed {
		return nil
	}
	d.subbed = false
	if err := d.bus.Unsubscribe(d.sub); err != nil {
		return fmt.Errorf("arrival.Detector.Stop: %w", err)
	}
	return nil
}

// handleFrame is the per-frame path, and it is deliberately cheap: two map
// lookups and a haversine for the overwhelming majority of frames, which come
// from cars that are not on a ride at all. Only one frame per CandidateTTL pays
// for a database read, and only a completed dwell pays for a write.
func (d *Detector) handleFrame(evt events.Event) {
	te, ok := evt.Payload.(events.VehicleTelemetryEvent)
	if !ok || te.VIN == "" {
		return
	}
	// Frames are accepted from BOTH sources. A REST backfill frame (MYR-394,
	// published while a ride is live for a car that is not streaming) carries
	// the same position and speed, and for a car that never streams it is the
	// only way this feature can work at all. The dwell being measured on frame
	// timestamps is what makes the two cadences interchangeable.
	fix, ok := fixFrom(te)
	if !ok {
		return
	}

	byVIN, fresh := d.candidates.ensure(d.ctx, d.now())
	if fresh {
		d.pruneTracks(byVIN)
	}
	cand, ok := byVIN[te.VIN]
	if !ok {
		return
	}

	tr := d.tracks[cand.RideRequestID]
	if tr == nil {
		tr = &track{}
		d.tracks[cand.RideRequestID] = tr
	}
	if tr.latched {
		return
	}
	if !tr.observe(fix, cand.PickupLatitude, cand.PickupLongitude, d.cfg) {
		return
	}

	// Latch BEFORE the write, release only if the write could not be attempted
	// to a conclusion. A refusal keeps the latch: the ride is somebody else's
	// now, and retrying it on each of the next twenty frames would be a
	// pointless UPDATE storm against a row that will keep refusing.
	tr.latched = true
	if d.advance(cand, fix) == outcomeUnavailable {
		tr.latched = false
	}
}

// pruneTracks drops dwell state for rides that have left the candidate set —
// advanced (by this detector or by the owner), cancelled, or aged out.
//
// Only ever called with a FRESHLY READ snapshot. Pruning against a stale or
// empty one would discard a latch while the ride was still live, and the next
// qualifying frame would then re-attempt a write the database has already
// settled.
func (d *Detector) pruneTracks(byVIN map[string]Candidate) {
	if len(d.tracks) == 0 {
		return
	}
	live := make(map[string]struct{}, len(byVIN))
	for _, c := range byVIN {
		live[c.RideRequestID] = struct{}{}
	}
	for rideID := range d.tracks {
		if _, ok := live[rideID]; !ok {
			delete(d.tracks, rideID)
		}
	}
}

// outcome is what one arrival attempt resolved to. Only outcomeUnavailable
// releases the latch.
type outcome int

const (
	// outcomeAdvanced: this detector won the guarded write and published.
	outcomeAdvanced outcome = iota
	// outcomeRefused: the write matched no row — the owner's tap won, or the
	// ride moved on. Silent and final.
	outcomeRefused
	// outcomeUnavailable: the write could not be completed (database error,
	// timeout, shutdown). Says nothing about the ride, so the latch is released
	// and a later frame may try again.
	outcomeUnavailable
)

// advance runs the guarded transition and, on a win, publishes both events.
func (d *Detector) advance(cand Candidate, fix Fix) outcome {
	ctx, cancel := context.WithTimeout(d.ctx, d.cfg.Timeout)
	defer cancel()

	updated, err := d.writer.MarkArrived(ctx, cand.RideRequestID)
	switch {
	case errors.Is(err, ErrRefused):
		// The expected loser's branch of the owner-tap race. Debug, because
		// nothing is wrong: the ride IS picked up, just not by us.
		d.logger.Debug("auto-arrival: transition refused, another writer won",
			slog.String("ride_id", cand.RideRequestID))
		return outcomeRefused
	case err != nil:
		d.logger.Warn("auto-arrival: transition failed",
			slog.String("ride_id", cand.RideRequestID),
			slog.String("error", err.Error()))
		return outcomeUnavailable
	}

	d.logger.Info("auto-arrival: car reached the pickup, ride advanced to arrived",
		slog.String("ride_id", cand.RideRequestID),
		slog.String("vehicle_id", cand.VehicleID),
		slog.String("vin", redactVIN(cand.VIN)),
		slog.Duration("dwell", d.cfg.Dwell),
		slog.Float64("radius_meters", d.cfg.RadiusMeters),
		// The car's own number, recorded as corroboration for whoever reads
		// this line later. It took no part in the decision — see the package
		// doc and MYR-527. -1 means the frame did not carry it.
		slog.Float64("car_miles_to_arrival", milesOrMissing(fix.MilesToArrival)),
	)

	d.publish(ctx, events.NewEvent(withPreviousStatus(updated)))
	d.publish(ctx, events.NewEvent(events.RideWaypointArrivedEvent{
		RideRequestID: cand.RideRequestID,
		VehicleID:     cand.VehicleID,
		RiderID:       cand.RiderID,
		OwnerID:       cand.OwnerID,
		Waypoint:      events.WaypointPickup,
	}))
	return outcomeAdvanced
}

// withPreviousStatus stamps the from-status onto the status event. Split out so
// the one field this detector adds to the adapter's projection is visible.
func withPreviousStatus(e events.RideStatusChangedEvent) events.RideStatusChangedEvent {
	e.PreviousStatus = statusAccepted
	return e
}

// publish is fire-and-forget and drop-safe: the transition is already committed
// in the database by the time either event goes out, so a publish failure is
// logged and nothing is unwound. The rider's client refetches on reconnect.
func (d *Detector) publish(ctx context.Context, evt events.Event) {
	if d.bus == nil {
		return
	}
	if err := d.bus.Publish(ctx, evt); err != nil {
		d.logger.Warn("auto-arrival: publish failed",
			slog.String("topic", string(evt.Topic)),
			slog.String("error", err.Error()))
	}
}

// milesOrMissing renders a nullable corroborating value for the log line.
func milesOrMissing(v *float64) float64 {
	if v == nil {
		return -1
	}
	return *v
}
