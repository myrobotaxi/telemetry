// Tunables for the MYR-505 zero-touch setup completion, and the ONE wire value
// this endpoint adds to MYR-491's setup-state vocabulary.

package telemetry

import "time"

// SetupStateStreaming is a FOURTH member of the setupState enum that exists
// ONLY on the §7.23 complete-setup response. It is never emitted by
// `deriveSetupState`, so §7.0 and §7.1 keep exactly the three members MYR-491
// contracted.
//
// WHY THE ACTION SURFACE NEEDS A MEMBER THE READ SURFACES DO NOT. MYR-491 made
// a deliberate, correct choice for a passive read: `null` means "the server
// makes NO CLAIM", and its schema says in as many words that `null` is not
// `ready` and that a consumer MUST NOT paint a positive badge from it. That is
// right for a snapshot, where `null` covers a healthy car, an unexamined car
// and an expired claim alike, and the server has no idea which.
//
// It is exactly wrong as the answer to a REQUEST. A client that just asked
// "finish setting up this car" and receives "no claim" has learned nothing, and
// the one thing MYR-505 exists to produce is the moment the narration can say
// "Live!". Here the server DOES know: it looked at the car's status and
// freshness on this very request. So the action surface returns a positive
// member and the read surfaces do not — one vocabulary, one `SetupState` shape,
// a superset on the surface that has earned the claim.
//
// This is the widening MYR-491's own schema anticipated: "New members may be
// added in a minor version; consumers MUST treat an unrecognised value as
// 'setup is incomplete, reason unknown' and MUST NOT fail the decode." A client
// that somehow met `streaming` on a read surface would degrade safely.
const SetupStateStreaming = "streaming"

// Defaults for SetupCompletionConfig.
//
// The budget is sized against a HUMAN, not against a fleet. The owner is
// holding their phone, having just tapped Approve in the Tesla app, watching a
// progress card — so the sequence has to finish inside the patience of someone
// standing at their car, and 30 seconds is roughly where a progress indicator
// stops feeling like progress. That is also comfortably longer than Tesla needs
// to record a key enrollment, which is a database write on their side rather
// than anything the car participates in.
const (
	// defaultSetupProbeBudget is the total wall-clock spend on pairing probes.
	defaultSetupProbeBudget = 30 * time.Second
	// defaultSetupProbeInterval spaces the probes. With the budget above it
	// yields probes at roughly t=0s, 7s, 14s, 21s and 28s — a handful of cheap
	// reads, not a poll loop.
	defaultSetupProbeInterval = 7 * time.Second
	// defaultSetupMaxProbes caps the probe COUNT independently of the clock.
	// Belt and braces on purpose: the budget alone would let a
	// mis-tuned interval turn into a tight loop against Tesla's rate-limited
	// read path, and the count alone would let a slow Tesla stretch one request
	// far past the owner's attention.
	defaultSetupMaxProbes = 5
	// defaultSetupCallTimeout bounds any single Tesla call so one hung
	// round-trip cannot consume the whole budget.
	defaultSetupCallTimeout = 10 * time.Second
	// defaultSetupCooldown is the minimum spacing between Tesla-hitting
	// completions of the SAME vehicle.
	//
	// It is deliberately LONGER than the probe budget, and that is what makes
	// it a concurrency bound as well as a rate limit: a run can take at most
	// ProbeBudget, so a cooldown of twice that guarantees a second request for
	// the same car cannot overlap the first no matter how it is timed. One wake
	// per minute per vehicle is the resulting worst case for a client stuck in
	// a retry loop — and the forced push is bounded far more tightly still, by
	// the pairing epoch.
	defaultSetupCooldown = 60 * time.Second
	// defaultSetupCooldownBurst is 1. Unlike the §7.9 command cooldown (which
	// allows 2 so a UI firing lock+climate together is not rejected) there is
	// no legitimate reason to complete one car's setup twice at once.
	defaultSetupCooldownBurst = 1
)

// SetupCompletionConfig tunes the completion sequence.
type SetupCompletionConfig struct {
	// ProbeBudget is the total wall-clock time spent waiting for Tesla to
	// report the virtual key enrolled.
	ProbeBudget time.Duration
	// ProbeInterval is the gap between pairing probes.
	ProbeInterval time.Duration
	// MaxProbes caps the number of pairing probes per request.
	MaxProbes int
	// CallTimeout bounds a single Tesla call.
	CallTimeout time.Duration
}

// withDefaults fills zero fields. Negative values are treated as zero so a
// misconfiguration degrades to the default rather than to a hot loop against
// Tesla — the same posture FleetConfigReconcileConfig takes.
func (c SetupCompletionConfig) withDefaults() SetupCompletionConfig {
	if c.ProbeBudget <= 0 {
		c.ProbeBudget = defaultSetupProbeBudget
	}
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = defaultSetupProbeInterval
	}
	if c.MaxProbes <= 0 {
		c.MaxProbes = defaultSetupMaxProbes
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = defaultSetupCallTimeout
	}
	return c
}
