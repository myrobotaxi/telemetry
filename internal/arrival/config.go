package arrival

import "time"

// Config holds the detector's thresholds. Every value has a production default
// (DefaultConfig); withDefaults fills a zero field so a caller that only wants
// to override one knob — or a test that only cares about the dwell — cannot
// accidentally configure a zero radius.
//
// Enabled is the AUTO_ARRIVAL_ENABLED kill-switch, read at composition time. It
// lives here rather than being checked per frame: cmd/ simply does not
// construct the detector when it is false, so a disabled deployment costs
// nothing and holds no state.
type Config struct {
	// Enabled is the AUTO_ARRIVAL_ENABLED kill-switch (default true).
	Enabled bool
	// RadiusMeters is how close the car must be to the pickup coordinate.
	RadiusMeters float64
	// Dwell is how long it must stay there, stopped, before the ride is
	// advanced.
	Dwell time.Duration
	// StoppedSpeedMPH is the speed at or below which the car counts as
	// stopped.
	StoppedSpeedMPH float64
	// StillnessMeters is how far two consecutive fixes may be apart and still
	// count as the car not having moved, when the frames themselves report
	// neither speed nor gear (MYR-563).
	StillnessMeters float64
	// MaxStillnessGap is the longest interval that positional stillness may be
	// inferred across. Beyond it two identical fixes prove nothing about the
	// time between them.
	MaxStillnessGap time.Duration
	// CandidateTTL is how long one candidate snapshot is served before it is
	// refreshed from the database.
	CandidateTTL time.Duration
	// CandidateLimit caps one refresh.
	CandidateLimit int
	// Timeout bounds BOTH the candidate refresh query and the arrival write,
	// so neither can wedge the frame path.
	Timeout time.Duration
}

// Production defaults. The radius and the dwell are the two the client can feel
// and the two most likely to be retuned from field reports, so the reasoning
// for each is written down rather than left in a commit message.
const (
	// defaultRadiusMeters — 80 m is about a city block's kerb frontage. It has
	// to absorb both a consumer GPS fix (5-15 m on a good day, worse between
	// buildings) and the honest gap between a pickup PIN and where a car can
	// actually stop: a driveway apron, the far side of a divided street, the
	// second row of a car park. Tighter and a correctly-arrived car never
	// qualifies, which fails silently and is indistinguishable from the feature
	// not working. Wider starts to reach the far kerb of a wide junction, and
	// at that point "here" stops being true.
	defaultRadiusMeters = 80.0
	// defaultDwell — 20 s of continuous stillness inside the radius. This is
	// the whole defence against a car that merely PASSES the pickup: stopped at
	// the lights outside the rider's building, queued behind a bus, waiting to
	// turn. Those clear in a few seconds; a car that has actually arrived stays
	// put for minutes while the rider walks out. At a 1 s streaming cadence
	// that is ~20 frames of agreement, and at the MYR-394 REST poll's ~25 s
	// cadence it is two frames, which is why the dwell is measured off frame
	// TIMESTAMPS and not off a frame count.
	defaultDwell = 20 * time.Second
	// defaultStoppedSpeedMPH — 1 mph. Not zero: a stationary car's GPS-derived
	// speed jitters around zero, and demanding an exact 0.0 would make the
	// dwell restart at random. Well under walking pace, so nothing that is
	// actually moving qualifies.
	defaultStoppedSpeedMPH = 1.0
	// defaultStillnessMeters — 15 m, and the number is DERIVED rather than
	// picked. The speed rung already calls 1 mph stopped; 1 mph sustained across
	// the shortest interval this rung will accept a pair of fixes over (the
	// ~25 s MYR-394 poll cadence) is 11 m. Fifteen sits just above that, so the
	// positional rung and the speed rung agree about the same car rather than
	// contradicting each other, and it comfortably absorbs the few metres a
	// stationary vehicle's GPS fix wanders between reads. Anything that is
	// actually driving covers far more: even walking pace clears 33 m per cycle.
	defaultStillnessMeters = 15.0
	// defaultMaxStillnessGap — 90 s, three REST poll cycles. It tolerates two
	// missed ones (an asleep car answering 408, a Tesla hiccup, a redeploy)
	// while refusing to reason across a window long enough for a car to have
	// left and returned. Past it the run restarts from the later fix, so an
	// outage DELAYS an arrival rather than fabricating one.
	defaultMaxStillnessGap = 90 * time.Second
	// defaultCandidateTTL — 15 s. It bounds two things: how long after a ride
	// becomes dispatched before the detector can see it (irrelevant — the car
	// has a drive ahead of it), and how long a MYR-541 trip edit can leave the
	// detector measuring against the OLD pickup (the reason it is seconds and
	// not minutes).
	defaultCandidateTTL = 15 * time.Second
	// defaultCandidateLimit — 100 concurrent leg-1 rides fleet-wide. See
	// store.defaultArrivalCandidateLimit: the cap is a guard against a
	// pathological scan, not a rationing policy.
	defaultCandidateLimit = 100
	// defaultTimeout — 5 s, generous for one indexed read or one guarded
	// UPDATE, and short enough that a database stall cannot back the frame
	// subscription up for long. (A backed-up subscription drops its OLDEST
	// frames, which for arrival detection is the harmless direction.)
	defaultTimeout = 5 * time.Second
	// staleSnapshotFactor — how many TTLs a snapshot may keep being served
	// while refreshes are failing, before the detector gives up and serves an
	// empty one. Four (a minute at the default TTL) is long enough to ride out
	// a failover and short enough that the detector is not deciding arrivals
	// from a picture of the fleet nobody can confirm.
	staleSnapshotFactor = 4
)

// DefaultConfig returns the production thresholds with the feature ON.
func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		RadiusMeters:    defaultRadiusMeters,
		Dwell:           defaultDwell,
		StoppedSpeedMPH: defaultStoppedSpeedMPH,
		StillnessMeters: defaultStillnessMeters,
		MaxStillnessGap: defaultMaxStillnessGap,
		CandidateTTL:    defaultCandidateTTL,
		CandidateLimit:  defaultCandidateLimit,
		Timeout:         defaultTimeout,
	}
}

// withDefaults replaces zero-valued knobs with their defaults. Dwell is the one
// field where zero is a legitimate value (tests use it to fire on the first
// qualifying frame), so it is left alone; every other zero would be a
// misconfiguration rather than a choice.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.RadiusMeters <= 0 {
		c.RadiusMeters = d.RadiusMeters
	}
	if c.StoppedSpeedMPH <= 0 {
		c.StoppedSpeedMPH = d.StoppedSpeedMPH
	}
	if c.StillnessMeters <= 0 {
		c.StillnessMeters = d.StillnessMeters
	}
	if c.MaxStillnessGap <= 0 {
		c.MaxStillnessGap = d.MaxStillnessGap
	}
	if c.CandidateTTL <= 0 {
		c.CandidateTTL = d.CandidateTTL
	}
	if c.CandidateLimit <= 0 {
		c.CandidateLimit = d.CandidateLimit
	}
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	return c
}
