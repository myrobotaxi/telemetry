package retention

import "time"

// Config tunes ONE retention sweep. The same struct serves both — the drive
// sweep (MYR-439) and the ride sweep (MYR-447) — because they run the same
// engine (sweeper.go) over different tables and want the same knobs. Each
// sweep is wired with its OWN Config value rather than sharing one, so their
// kill-switches and paces stay independent: a misbehaving ride sweep can be
// stopped without also stopping the drive sweep. Zero values get defaults via
// withDefaults.
//
// NOTE WHAT IS NOT HERE: the retention windows. Every one of them — 365 days
// for drives (store.DriveRetentionDays), 365 days for terminal rides
// (store.RideRetentionDays), 30 days for the passenger-field scrub
// (store.RidePassengerScrubDays) — is a compile-time constant in the store
// layer, deliberately unreachable from configuration — CG-DL-4, because a
// window an operator can shorten by mistake is a fleet's history one typo from
// gone. Everything in this struct governs PACE, never SCOPE.
type Config struct {
	// Enabled is the kill-switch for whichever sweep this value configures:
	// DRIVE_RETENTION_PRUNER_ENABLED for the drive sweep,
	// RIDE_RETENTION_PRUNER_ENABLED for the ride sweep. Unlike the Live
	// Activity ticker's switch, this one stops the loop outright: pruning is
	// the whole job, so there is no useful degraded mode between "running" and
	// "off". Off means the rows that sweep is responsible for accumulate past
	// their window until it is turned back on — recoverable, because the sweep
	// is idempotent and the next pass simply finds more work.
	Enabled bool
	// RunAtUTC is the offset from UTC midnight at which each pass fires
	// (data-lifecycle.md §5.2: daily at 03:00 UTC).
	RunAtUTC time.Duration
	// BatchSize is how many drives one transaction deletes.
	BatchSize int
	// MaxBatchesPerPass bounds one pass so a pathological backlog cannot pin a
	// connection for hours. Overflow is not lost — the next pass resumes from
	// the same oldest-first position.
	MaxBatchesPerPass int
	// BatchTimeout bounds ONE batch transaction. A stalled statement fails its
	// batch and gets retried rather than wedging the pass.
	BatchTimeout time.Duration
}

const (
	// defaultRunAtUTC is 03:00 UTC per data-lifecycle.md §5.2 — a low-traffic
	// window for the fleet, and well clear of the daily Tesla token refresh.
	defaultRunAtUTC = 3 * time.Hour
	// defaultBatchSize mirrors store.DefaultPruneBatchSize (§5.5). Named again
	// here rather than imported so that internal/retention keeps its
	// consumer-site-interface independence from internal/store; the store side
	// validates its own argument regardless.
	defaultBatchSize = 100
	// defaultMaxBatchesPerPass is 500 batches — 50,000 drives at the default
	// batch size.
	//
	// THIS IS HEADROOM, NOT A RESPONSE TO AN EXISTING BACKLOG. As of 2026-08-02
	// production holds 315 Drive rows, the oldest created 2026-03-08, so NOTHING
	// is eligible for deletion until 2027-03-08 and every pass until then
	// deletes zero rows. The budget exists because a ceiling is cheaper to have
	// than to add later, and because the shape of the backlog on the day the
	// window first bites is not knowable now — not because the first pass has
	// work to do. A budget-capped pass logs a warning naming the cap, which is
	// the signal to raise it.
	defaultMaxBatchesPerPass = 500
	// defaultBatchTimeout bounds one batch transaction. Generous for a
	// 100-row delete on an indexed predicate; short enough that a wedged
	// statement is retried the same night rather than the next one.
	defaultBatchTimeout = 30 * time.Second

	// pruneAttempts is the per-batch try count (§5.6: "3 attempts max").
	pruneAttempts = 3
	// pruneBackoffBase is the first inter-attempt wait; it doubles each time,
	// giving 1s then 2s across the three attempts of §5.6.
	pruneBackoffBase = time.Second
	// runJitter spreads the wake-up across a five-minute window so replicas do
	// not all start claiming batches on the same second. It is an ABSOLUTE
	// spread, not a fraction of the ~24h wait, which would move the run hours
	// off the 03:00 slot the contract specifies. Correctness does not depend on
	// it — the claim query's SKIP LOCKED already makes concurrent replicas take
	// disjoint batches — so this is purely about not stampeding the pool.
	runJitter = 5 * time.Minute
)

func (c Config) withDefaults() Config {
	if c.RunAtUTC <= 0 {
		c.RunAtUTC = defaultRunAtUTC
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.MaxBatchesPerPass <= 0 {
		c.MaxBatchesPerPass = defaultMaxBatchesPerPass
	}
	if c.BatchTimeout <= 0 {
		c.BatchTimeout = defaultBatchTimeout
	}
	return c
}
