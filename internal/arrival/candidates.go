package arrival

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// Candidate is one ride whose car is currently driving to one of that ride's
// waypoints. Built by the cmd/ adapter from the store's lean projection; the
// detector needs the VIN to match frames, the target to measure against, the
// waypoint label to say WHICH place was reached, and the ids to write and
// publish with.
//
// Since MYR-539 the waypoint is not always the pickup: a ride under way is
// watched against its CURRENT stop, or against the destination when no stop is
// left ahead of the car.
//
// The target coordinate is P1 GPS data — never logged.
type Candidate struct {
	RideRequestID string
	VehicleID     string
	RiderID       string
	OwnerID       string
	VIN           string
	// Waypoint is the events vocabulary: "pickup", "stop:{id}",
	// "destination". It decides what an arrival MEANS — only the pickup moves
	// the ride's lifecycle status.
	Waypoint        string
	TargetLatitude  float64
	TargetLongitude float64
}

// isPickup reports whether this candidate is watching leg 1's endpoint — the
// only waypoint whose arrival writes a ride status.
func (c Candidate) isPickup() bool { return c.Waypoint == events.WaypointPickup }

// trackKey identifies the dwell state for this candidate. It is keyed by
// (ride, waypoint) rather than by ride alone (MYR-539): one ride now reaches
// several places in sequence, and a latch keyed only by ride would silence
// every stop after the first.
func (c Candidate) trackKey() string { return c.RideRequestID + "|" + c.Waypoint }

// CandidateStore is the detector's read seam, satisfied by the ride-request
// repo through a cmd/ adapter.
type CandidateStore interface {
	// ListArrivalCandidates returns the rides whose car is driving to a
	// waypoint — `accepted` with leg-1 dispatch resolved `sent` (the pickup),
	// or `enroute` (the current stop, else the destination) — capped at limit,
	// target decrypted.
	ListArrivalCandidates(ctx context.Context, limit int) ([]Candidate, error)
}

// candidateCache serves the candidate set to the frame path as a VIN-keyed map,
// refreshing it from the store no more often than the TTL.
//
// The refresh happens LAZILY, on the frame path, rather than on a background
// ticker. That means a fleet where nothing is streaming costs zero queries
// (rather than one every 15s forever), and it means the snapshot is only ever
// as old as the TTL at the moment it is actually used. The cost is that one
// frame in every TTL pays for a query on the subscription goroutine — bounded
// by Config.Timeout, and if it backs the subscription up, the bus drops the
// OLDEST frames, which for a dwell measured on frame timestamps is the harmless
// direction.
//
// Not internally locked: every method is called from the single handler
// goroutine the bus delivers on.
type candidateCache struct {
	store  CandidateStore
	cfg    Config
	logger *slog.Logger

	// byVIN is the current snapshot. nil before the first refresh.
	byVIN map[string]Candidate
	// fetchedAt is when byVIN was installed; refreshedAt is when a refresh was
	// last ATTEMPTED. They diverge while refreshes are failing, and the gap is
	// what the staleness ceiling measures.
	fetchedAt time.Time
	// failing records that the last refresh attempt returned an error, so the
	// log line for the next success can say the outage ended.
	failing bool
	// fingerprint is the composition of the last snapshot installed, so a
	// rebuild is logged only when the set actually changed (MYR-563).
	fingerprint string

	// dirty is set from ANOTHER goroutine — the trip-edit subscription — to
	// force the next frame to re-read. It is the one piece of this struct not
	// owned by the frame handler, which is exactly why it is an atomic flag and
	// not a field: the snapshot map itself stays single-goroutine, so no lock
	// is introduced onto the per-frame path.
	dirty atomic.Bool
}

// invalidate marks the snapshot stale so the next frame re-derives it. Called
// from the trip-edit seam (MYR-563): a mid-leg edit re-points the CAR
// immediately, and the detector measuring against the previous target until the
// TTL happens to lapse is a window in which the two disagree about where the
// ride is going. Cheap by design — it schedules a read, it does not perform one,
// so a burst of edits costs one refresh rather than one each.
func (c *candidateCache) invalidate() { c.dirty.Store(true) }

// ensure returns the VIN-keyed candidate snapshot, refreshing it first when the
// current one has aged past the TTL. The second return reports whether THIS
// call installed a fresh snapshot — the detector prunes its per-ride tracks
// only against those, never against a stale or empty one.
//
// WHAT HAPPENS WHEN THE DATABASE IS UNREACHABLE, and why it is the safe
// direction. The previous snapshot keeps being served for up to
// staleSnapshotFactor TTLs, because a brief blip must not make the fleet
// disappear mid-dwell. Past that ceiling the cache serves an EMPTY map: no
// candidates, no arrivals, no tracks. Failing towards "detect nothing" is the
// only correct choice here — a stale candidate list cannot be checked against
// a cancel, a trip edit, or an owner's tap, and every one of those would turn
// an auto-arrival into a wrong one. The owner's manual tap is unaffected by any
// of this, which is what makes the degradation cheap.
func (c *candidateCache) ensure(ctx context.Context, now time.Time) (map[string]Candidate, bool) {
	// A trip edit forces the read regardless of age, and the flag is cleared by
	// the attempt rather than by its success: a failed refresh falls through to
	// the ordinary staleness rules below, which already know what to do, and
	// leaving it set would pin the detector to a read-every-frame loop for as
	// long as the database stayed down.
	forced := c.dirty.Swap(false)
	if !forced && c.byVIN != nil && now.Sub(c.fetchedAt) < c.cfg.CandidateTTL {
		return c.byVIN, false
	}

	readCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	cands, err := c.store.ListArrivalCandidates(readCtx, c.cfg.CandidateLimit)
	if err != nil {
		return c.serveStale(now, err), false
	}

	if c.failing {
		c.logger.Info("auto-arrival: candidate refresh recovered",
			slog.Duration("outage", now.Sub(c.fetchedAt)))
		c.failing = false
	}
	c.byVIN = indexByVIN(cands, c.logger)
	c.fetchedAt = now
	if fp := candidateSetFingerprint(c.byVIN); fp != c.fingerprint {
		c.fingerprint = fp
		c.logger.Info("auto-arrival: candidate set rebuilt",
			slog.Int("candidates", len(c.byVIN)),
			slog.Bool("forced_by_trip_edit", forced),
			slog.String("watching", fp))
	}
	return c.byVIN, true
}

// serveStale decides what to hand back after a failed refresh: the previous
// snapshot while it is inside the staleness ceiling, an empty one past it.
func (c *candidateCache) serveStale(now time.Time, err error) map[string]Candidate {
	age := now.Sub(c.fetchedAt)
	ceiling := time.Duration(staleSnapshotFactor) * c.cfg.CandidateTTL
	if c.byVIN != nil && age <= ceiling {
		// Debug, not Warn: one blip inside the ceiling changes no behaviour,
		// and this is on a path that can run once per TTL for hours.
		c.logger.Debug("auto-arrival: candidate refresh failed, serving cached set",
			slog.Duration("snapshot_age", age),
			slog.String("error", err.Error()))
		c.failing = true
		return c.byVIN
	}
	// Warned once per outage: on the failure that DISCARDS a snapshot, and on
	// the first failure when there was none to discard. Repeat failures past
	// that are silent — the state is already reported and the path repeats
	// every TTL.
	if c.byVIN != nil || !c.failing {
		c.logger.Warn("auto-arrival: candidate set unavailable, detection suspended",
			slog.Duration("snapshot_age", age),
			slog.String("error", err.Error()))
	}
	c.failing = true
	c.byVIN = nil
	return map[string]Candidate{}
}

// indexByVIN keys the candidate rows by VIN, DROPPING any VIN that carries more
// than one candidate ride.
//
// One car with two live rides should not be reachable — migration 0013's
// partial unique index admits a single active instant ride per vehicle — but
// "should not" is not the standard here. If it ever happens, the detector
// cannot tell which of the two rides the car being at a pickup means, and
// guessing would advance the wrong rider's ride. Both are dropped, both owners
// keep their tap, and the ambiguity is logged rather than resolved.
func indexByVIN(cands []Candidate, logger *slog.Logger) map[string]Candidate {
	byVIN := make(map[string]Candidate, len(cands))
	ambiguous := make(map[string]struct{})
	for i := range cands {
		vin := cands[i].VIN
		if _, dup := byVIN[vin]; dup {
			ambiguous[vin] = struct{}{}
			continue
		}
		byVIN[vin] = cands[i]
	}
	for vin := range ambiguous {
		delete(byVIN, vin)
		logger.Warn("auto-arrival: vehicle has multiple live rides, skipping",
			slog.String("vin", redactVIN(vin)))
	}
	return byVIN
}

// redactVIN keeps the last 4 characters only — the CLAUDE.md production-logging
// rule (VINs are P1).
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return "****"
	}
	return "****" + vin[len(vin)-4:]
}
