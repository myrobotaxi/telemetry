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

// Detector watches decoded telemetry frames and reports that a ride's car has
// reached one of that ride's waypoints — advancing the ride to `arrived` when
// that waypoint is the PICKUP, and publishing the observation alone for a stop
// or the destination (MYR-539). See the package doc for the rule and for why it
// is tuned to fire late rather than early.
type Detector struct {
	bus        events.Bus
	cfg        Config
	candidates *candidateCache
	writer     Writer
	logger     *slog.Logger

	// tracks holds per-WAYPOINT dwell state, keyed by Candidate.trackKey (ride
	// + waypoint). A plain map with no mutex: the bus delivers serially per
	// subscription, so this and the cache are only ever touched from the
	// handler goroutine.
	tracks map[string]*track

	// now is the clock used for candidate-cache ageing ONLY. The dwell is
	// measured off frame timestamps (see Fix.At), never off this.
	now func() time.Time

	// subs are the detector's two subscriptions: the telemetry frames it
	// decides on, and the trip-edit seam that tells it the decision's premises
	// changed.
	subs   []events.Subscription
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

	seams := []struct {
		topic   events.Topic
		handler events.Handler
	}{
		{events.TopicVehicleTelemetry, d.handleFrame},
		{events.TopicRideTripChanged, d.handleTripChanged},
	}
	for _, s := range seams {
		sub, err := d.bus.Subscribe(s.topic, s.handler)
		if err != nil {
			d.cancel()
			// Detach whatever DID attach, so a half-started detector cannot be
			// left holding a live subscription. Its own failure is discarded:
			// the caller is already being told why Start failed, and a second
			// error would only bury the first.
			_ = d.unsubscribeAll()
			return fmt.Errorf("arrival.Detector.Start(%s): %w", s.topic, err)
		}
		d.subs = append(d.subs, sub)
	}
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
	return d.unsubscribeAll()
}

// unsubscribeAll drops every subscription, reporting the FIRST failure while
// still detaching the rest: a half-unsubscribed detector would keep receiving
// frames into a cancelled context.
func (d *Detector) unsubscribeAll() error {
	subs := d.subs
	d.subs = nil
	var firstErr error
	for _, sub := range subs {
		if err := d.bus.Unsubscribe(sub); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("arrival.Detector.Stop: %w", err)
		}
	}
	return firstErr
}

// handleTripChanged forces the candidate set to be re-derived on the next
// frame, and it is the direct answer to the shape MYR-563 was reported in: a
// stop added MID-LEG re-points the car within seconds, so the detector must not
// spend the rest of a TTL measuring the car against the place the trip no longer
// ends at.
//
// It does NO work of its own — no read, no map touch, one atomic store — which
// is what keeps the snapshot single-goroutine. The bus delivers this seam on a
// different goroutine from the frames, and a handler here that touched the
// cache directly would be racing the frame path for the sake of saving 15
// seconds.
//
// The correctness of detection never depended on this: the set is re-derived
// from CURRENT ride and stop state every TTL regardless, and a fresh process
// holds no snapshot at all, which together are why a ride already stuck at its
// stop recovers on the first fixes after a deploy with nobody editing anything.
// This narrows the window; it is not what closes it.
func (d *Detector) handleTripChanged(evt events.Event) {
	if _, ok := evt.Payload.(events.RideTripChangedEvent); !ok {
		return
	}
	d.candidates.invalidate()
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
	// Frames are accepted from BOTH sources, and for a car that has PARKED the
	// REST backfill (MYR-394) is not a fallback but the only source there is —
	// Tesla stops streaming at the shift to P, which is the exact moment this
	// detector exists to notice. Such a frame carries a location and nothing
	// else (no speed, no gear), which is why the stillness ladder has a
	// positional rung; see Fix.stillSinceGiven. The dwell being measured on
	// frame timestamps is what makes the two cadences interchangeable.
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

	key := cand.trackKey()
	tr := d.tracks[key]
	if tr == nil {
		tr = &track{}
		d.tracks[key] = tr
		d.noteVerdict(cand, tr, verdictArmed, observation{})
	}
	if tr.latched {
		return
	}
	obs := tr.observe(fix, cand.TargetLatitude, cand.TargetLongitude, d.cfg)
	if !obs.dwellMet {
		d.noteVerdict(cand, tr, obs.verdict(), obs)
		return
	}
	d.noteVerdict(cand, tr, verdictDwellSatisfied, obs)

	// Latch BEFORE the write, release only if the write could not be attempted
	// to a conclusion. A refusal keeps the latch: the ride is somebody else's
	// now, and retrying it on each of the next twenty frames would be a
	// pointless UPDATE storm against a row that will keep refusing.
	tr.latched = true
	switch d.advance(cand, fix) {
	case outcomeAdvanced:
		d.noteVerdict(cand, tr, verdictPublished, obs)
	case outcomeRefused:
		d.noteVerdict(cand, tr, verdictSuppressedRefused, obs)
	case outcomeUnavailable:
		d.noteVerdict(cand, tr, verdictSuppressedUnavailable, obs)
		tr.latched = false
	}
}

// pruneTracks drops dwell state for (ride, waypoint) pairs that have left the
// candidate set — advanced (by this detector, by the leg advance, or by the
// owner), cancelled, or aged out. A ride whose CURRENT stop moved on leaves its
// old waypoint behind here and starts a fresh track for the new one, which is
// exactly how one ride comes to detect several arrivals in sequence.
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
		live[c.trackKey()] = struct{}{}
	}
	for key := range d.tracks {
		if _, ok := live[key]; !ok {
			delete(d.tracks, key)
		}
	}
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
