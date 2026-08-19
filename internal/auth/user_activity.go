package auth

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// The LAST-SEEN STAMP (MYR-592). One hook, both surfaces.
//
// ── WHY IT LIVES ON ValidateToken AND NOT IN A MIDDLEWARE ───────────────────
//
// The brief said "hook the authenticated request path ONCE, covering REST and
// the WebSocket handshake". This server has no central REST auth middleware to
// hook — `internal/server` wraps exactly one thing, a request logger, and each
// of the ~30 REST handlers authenticates itself by calling ValidateToken
// directly. The WebSocket handshake does not carry an Authorization header at
// all: the token arrives in the first frame's JSON body and is resolved by
// ws.Hub.authenticateClient. The two surfaces share exactly ONE function, and
// this is it. Stamping anywhere else would mean either thirty call sites or two
// hooks that drift.
//
// It also happens to be the most honest definition of the signal. "The account
// was active" means a bearer belonging to it was accepted — not that a socket is
// open, not that a device token is registered, not that a row was updated. Those
// were the three candidates the schema already had, and each of them is true for
// people who have not opened the app in weeks.
//
// ── EVERY FAILURE IS SWALLOWED, DELIBERATELY ────────────────────────────────
//
// This runs INSIDE token validation. A last-seen write must never fail an
// authentication, so Stamp returns nothing and logs at Warn. The blast radius of
// the alternative is the whole product: a transient write error on a bookkeeping
// table would sign every user out. The cost of swallowing is that an owner looks
// staler than they are, and the sweeper's response to "staler" tops out at a
// warning push a day before anything irreversible happens.
//
// ── THE STAMP IS DETACHED FROM THE REQUEST'S CANCELLATION ───────────────────
//
// The write runs on context.WithoutCancel with its own short timeout, the same
// discipline internal/push uses to drop an unregistered device token. A client
// that hangs up mid-request has still authenticated, and the value we are
// recording is precisely that fact — letting the disconnect cancel the write
// would systematically under-count the least patient users.
//
// ── TWO THROTTLES, NOT ONE ──────────────────────────────────────────────────
//
// The in-process gate below short-circuits so the statement is usually not
// issued at all; the SQL `ON CONFLICT … WHERE` guard in
// store.UserActivityRepo.Touch is the durable backstop that survives restarts
// and holds across replicas. Neither alone is sufficient: the memory gate is
// lost on deploy (and every replica has its own), and reaching the database on
// every authenticated request to be told "no" is most of the cost the throttle
// exists to avoid.
//
// The gate is marked only on a SUCCESSFUL write. Marking optimistically would
// convert one failed write into a full throttle window of silence for that
// account — the failure would hide itself for an hour, which for a value read
// against a four-day threshold is the wrong direction to be quiet in.

// activityThrottle is how long a stamped account is left alone.
//
// One hour, against thresholds of four and five DAYS. The stored value is
// therefore at most 1/96th of the warning threshold stale, and always stale in
// the safe direction (an active owner reads as slightly less recent than they
// are, never the reverse). Widening this is a pure cost/precision trade with no
// correctness argument attached until it approaches a day.
const activityThrottle = time.Hour

// activityWriteTimeout bounds the detached write. Short on purpose: this is a
// single-row upsert on a primary key, and a stamp that has not landed in two
// seconds is a database problem, not a slow query. Failing fast keeps the
// goroutine from outliving the request by any meaningful amount.
const activityWriteTimeout = 2 * time.Second

// ActivityWriter is the consumer-site view of the last-seen store.
// Satisfied by *store.UserActivityRepo.
//
// It takes the throttle rather than owning one so that the SQL guard and the
// in-process gate are provably the same window — one constant, passed down,
// instead of two that agree today.
type ActivityWriter interface {
	Touch(ctx context.Context, userID string, now time.Time, throttle time.Duration) (bool, error)
}

// activityStamper is the JWTAuthenticator's view of the stamp. Unexported and
// one method, so the authenticator cannot accidentally grow a dependency on the
// store package.
type activityStamper interface {
	Stamp(ctx context.Context, userID string)
}

// UserActivityStamper records that an account authenticated, at most once per
// throttle window per process.
type UserActivityStamper struct {
	writer   ActivityWriter
	logger   *slog.Logger
	throttle time.Duration
	now      func() time.Time

	// seen maps userID -> the last instant this process successfully stamped.
	// sync.Map rather than a mutex-guarded map because the access pattern is
	// overwhelmingly read-mostly on a stable key set, which is the case
	// sync.Map documents itself for — and it is the same choice
	// userExistenceCache next door already made for the same reason.
	//
	// UNBOUNDED BY DESIGN, and the bound is external: one entry per account
	// that has authenticated against this process since it started, each a
	// string key and a time.Time. A fleet of a million accounts would cost
	// tens of megabytes; the actual population is the beta's. If that ever
	// stops being true the fix is an eviction sweep, not a smaller window —
	// shrinking the window would put the cost back on the database.
	seen sync.Map
}

// NewUserActivityStamper wires a stamper. A nil writer yields a stamper that
// does nothing, which is the ordinary unwired state in tests and in --dev mode
// rather than an error: the platform must run without this table.
func NewUserActivityStamper(writer ActivityWriter, logger *slog.Logger) *UserActivityStamper {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &UserActivityStamper{
		writer:   writer,
		logger:   logger,
		throttle: activityThrottle,
		now:      time.Now,
	}
}

// withThrottle overrides the window. Test seam only — production always uses
// activityThrottle, so that the SQL guard and the memory gate cannot disagree.
func (s *UserActivityStamper) withThrottle(d time.Duration) *UserActivityStamper {
	if d > 0 {
		s.throttle = d
	}
	return s
}

// withClock injects a clock for deterministic throttle tests.
func (s *UserActivityStamper) withClock(now func() time.Time) *UserActivityStamper {
	if now != nil {
		s.now = now
	}
	return s
}

// Stamp records that userID authenticated. It never returns an error and never
// blocks on the caller's context being cancelled.
//
// SYNCHRONOUS BY CHOICE. At most one write per account per hour reaches the
// database, so the latency this adds to a request is a single-row upsert
// amortised over an hour of that account's traffic — unmeasurable. A goroutine
// would buy nothing and would cost the property that makes this testable: after
// Stamp returns, the write has either happened or been deliberately skipped.
func (s *UserActivityStamper) Stamp(ctx context.Context, userID string) {
	if s == nil || s.writer == nil || userID == "" {
		return
	}

	now := s.now()
	if s.recentlyStamped(userID, now) {
		return
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activityWriteTimeout)
	defer cancel()

	if _, err := s.writer.Touch(writeCtx, userID, now, s.throttle); err != nil {
		// Logged, never returned: see the file header. The user id is P0 and
		// safe here; the last-seen VALUE is P1 and is not logged.
		s.logger.Warn("last-seen stamp failed (authentication unaffected)",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return
	}

	s.seen.Store(userID, now)
}

// recentlyStamped reports whether this process already stamped userID inside the
// throttle window.
//
// THE WINDOW IS HALF-OPEN AT BOTH ENDS, and the lower end is the interesting
// one. A naive `now.Sub(last) < throttle` also matches NEGATIVE elapsed times —
// an entry stamped in the future, which an NTP step backwards produces — and
// would then pin that account shut until real time caught up, potentially hours.
// Requiring `elapsed > 0` makes a backwards clock re-open the gate instead of
// closing it, and the SQL guard absorbs the extra write.
func (s *UserActivityStamper) recentlyStamped(userID string, now time.Time) bool {
	v, ok := s.seen.Load(userID)
	if !ok {
		return false
	}
	last, ok := v.(time.Time)
	if !ok {
		return false
	}
	elapsed := now.Sub(last)
	return elapsed > 0 && elapsed < s.throttle
}

// discardWriter satisfies io.Writer for the nil-logger fallback, matching the
// pattern internal/dispatch and internal/retention already use.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
