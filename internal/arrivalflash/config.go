package arrivalflash

import "time"

// Config tunes the greeting. Every zero-valued knob is replaced by its
// production default (withDefaults), so a caller overriding one — or a test
// that only cares about the spacing — cannot accidentally configure zero
// flashes or a zero timeout.
//
// Enabled is the ARRIVAL_FLASH_ENABLED kill-switch, read once at Start rather
// than per event: a disabled deployment never subscribes, so it holds no state
// and costs nothing.
type Config struct {
	// Enabled is the ARRIVAL_FLASH_ENABLED kill-switch (default true).
	Enabled bool
	// Flashes is how many times the lights are flashed per arrival. Also the
	// HARD CEILING on Tesla command attempts for one waypoint: a flash is
	// never retried, so attempts can never exceed this.
	Flashes int
	// Interval is the gap between consecutive flashes.
	Interval time.Duration
	// Timeout bounds the WHOLE greeting — VIN resolve, token resolve, and all
	// three flashes with their gaps — so a wedged Tesla call can neither
	// outlive the ride nor hold up shutdown indefinitely.
	Timeout time.Duration
	// LatchSize caps the exactly-once latch. See latch.go for why a bound is
	// safe here and what is lost at the far edge of it.
	LatchSize int
}

// Production defaults. The count and the spacing are the two the client chose
// and the two a person can actually see, so the reasoning is written down
// rather than left in a commit message.
const (
	// defaultFlashes — THREE, client-decided. It is also the number that reads
	// as deliberate: one flash is indistinguishable from a passing reflection,
	// two from a glitch, and three is unmistakably a car saying something. More
	// than three starts to look like a hazard signal.
	defaultFlashes = 3
	// defaultInterval — 1.5 s. Fast enough that the three land as one gesture
	// rather than three unrelated events, slow enough that a rider glancing up
	// on the second one still sees the third. It is also comfortably longer
	// than a signed command round-trip, so the flashes are paced by this and
	// not by the network.
	defaultInterval = 1500 * time.Millisecond
	// defaultTimeout — 90 s. The three flashes and their two gaps are ~3 s of
	// wall clock; the rest of the budget belongs to the FIRST command, which
	// may spend the executor's own wake budget (3 attempts, 2 s backoff, plus
	// the car actually booting) on a car that dozed off at the kerb. Generous
	// enough to cover that, and bounded so shutdown's drain has an end.
	defaultTimeout = 90 * time.Second
	// defaultLatchSize — 512 waypoints. Orders of magnitude above any plausible
	// concurrent-ride count for this fleet, so the eviction path is dead code
	// in practice and exists only so a long-lived process cannot grow the map
	// without limit.
	defaultLatchSize = 512
)

// DefaultConfig returns the production settings with the greeting ON.
func DefaultConfig() Config {
	return Config{
		Enabled:   true,
		Flashes:   defaultFlashes,
		Interval:  defaultInterval,
		Timeout:   defaultTimeout,
		LatchSize: defaultLatchSize,
	}
}

// withDefaults replaces zero-valued knobs with their defaults. Interval is the
// one field where zero is legitimate — tests use it to run the sequence without
// waiting — so it is left alone; every other zero would be a misconfiguration
// rather than a choice, and a zero Flashes in particular would silently turn
// the feature off through a path that is not the kill-switch.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Flashes <= 0 {
		c.Flashes = d.Flashes
	}
	if c.Interval < 0 {
		c.Interval = d.Interval
	}
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.LatchSize <= 0 {
		c.LatchSize = d.LatchSize
	}
	return c
}
