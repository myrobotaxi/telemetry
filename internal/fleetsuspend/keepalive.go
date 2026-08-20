package fleetsuspend

// The KEEPALIVE arm (MYR-594). MYR-592's two arms take things away; this one
// gives nothing back and exists to stop the taking from becoming permanent.
//
// ── THE FAILURE IT PREVENTS ─────────────────────────────────────────────────
//
// Tesla's refresh tokens are SINGLE-USE, ROTATING, and they LAPSE after roughly
// three months of non-use. Every refresh this platform performs is on-demand:
// something wanted an access token, found it expired, and rotated. MYR-592 then
// created the one population that wants nothing — an owner whose vehicles are
// SUSPENDED makes no Tesla calls at all, by design, until they come back. Leave
// that owner alone for three months and their refresh token lapses, at which
// point the one-tap reconnect that suspension promised degrades into a full
// OAuth re-pair with a virtual-key dance. The cost saving would have been paid
// for with the thing it was supposed to protect.
//
// So this arm rotates a dormant owner's grant ONCE, deliberately, at 45 days —
// half the lapse window, which leaves room for a fortnight of failed passes
// before anything is actually at risk.
//
// ── WHY IT IS SAFE TO DO THIS BEHIND THE OWNER'S BACK ───────────────────────
//
// A rotation is not a state change anybody can see. It does not un-suspend a
// car, touch a config at Tesla, publish an event, notify a phone or alter a
// single row the contract exposes. The owner's next reconnect finds a grant
// that works, which is the same thing it would have found on day one.
//
// ── THE HAZARD, AND WHY THIS ARM CANNOT CAUSE IT ────────────────────────────
//
// Single-use rotation means two callers holding the same refresh token produce
// one winner and one `invalid_grant`. A keepalive that raced the reconnect
// endpoint into a dead token would be far worse than the lapse it prevents.
// Three things stop it, in order of how much they carry:
//
//  1. NEITHER PATH WRITES ON FAILURE. The on-demand handlers store a pair only
//     after Tesla returns one, and store.RotateTeslaTokenLocked aborts its
//     transaction on any rotation error. So the loser of a race writes nothing
//     and the database always ends up holding the pair TESLA honoured. A race
//     can cost a user one honest error; it cannot strand an account.
//
//  2. THE ROTATION IS LOCKED AND ABANDONS CONTENTION. The read and the write
//     are one transaction over a `FOR UPDATE NOWAIT` row lock, so no other
//     writer can commit between them, and if anybody already holds that lock we
//     return ErrRotationBusy having never even READ the refresh token. Skipping
//     costs one pass against a 45-day margin.
//
//  3. THE POPULATION IS DISJOINT FROM THE CONTENDERS. The candidate query wants
//     an owner whose cars are suspended AND who has not authenticated for as
//     long as suspension itself requires. The on-demand path is driven by that
//     same person's authenticated requests, of which they are making none. The
//     activity gate is not a second staleness test — the token clock already
//     did that job — it is there purely to keep this arm away from anybody who
//     has just woken up and might be reaching for the reconnect button.
//
// ── WHAT A REFUSAL DOES: NOTHING, LOUDLY, THEN NOTHING AGAIN FOR A WEEK ─────
//
// A grant Tesla refuses is lapsed or revoked, and no retry cures either. The
// stored token is LEFT EXACTLY AS IT IS — it is the only evidence of which
// account needs re-pairing, and clobbering it would buy nothing. The refusal is
// logged once and recorded, and the record is what stops the arm asking again
// every hour for the rest of the platform's life. The user-facing half of this
// is the reconnect sheet's honest failure, which already exists.

import (
	"context"
	"errors"
	"time"
)

// ErrRotationBusy — another writer holds the owner's OAuth row, so the rotation
// was not attempted and no refresh token was read. Declared here at the consumer
// site (like every other interface in this package) and mapped onto by the cmd/
// adapter, so internal/fleetsuspend never imports internal/store.
var ErrRotationBusy = errors.New("tesla token rotation busy")

// KeepaliveCandidate is one dormant owner whose grant is going stale.
type KeepaliveCandidate struct {
	// OwnerID is the account whose grant is rotated. The only id this arm logs.
	OwnerID string
	// TokenExpiresAt is the grant's stored expiry — the rotation clock the
	// staleness gate fired on. Logged as evidence, never as a deadline.
	TokenExpiresAt time.Time
	// TokenExpiryUnix is the same value as the column holds it, pinned to a
	// failure record so the cooldown can be released early if anything rotates
	// the grant in the meantime.
	TokenExpiryUnix int64
}

// KeepaliveQuery is the four thresholds and the cap one pass reads with.
type KeepaliveQuery struct {
	StaleBefore   time.Time
	QuietBefore   time.Time
	AttemptBefore time.Time
	FailureBefore time.Time
	Limit         int
}

// KeepaliveStore is the persistence surface this arm needs.
type KeepaliveStore interface {
	// ListStaleTokenOwners returns due owners, stalest grant first.
	ListStaleTokenOwners(ctx context.Context, q KeepaliveQuery) ([]KeepaliveCandidate, error)
	// RecordKeepaliveSuccess stamps a rotation that landed and clears any
	// outstanding failure.
	RecordKeepaliveSuccess(ctx context.Context, userID string, at time.Time) error
	// RecordKeepaliveFailure stamps a refusal, pinned to the token generation
	// it failed on, and starts the cooldown.
	RecordKeepaliveFailure(ctx context.Context, userID string, at time.Time, tokenExpiryUnix int64) error
}

// TokenRotator spends one owner's stored refresh token and stores what comes
// back, under a row lock. Satisfied by an adapter over
// store.AccountRepo.RotateTeslaTokenLocked plus the SAME telemetry.TokenRefresher
// the on-demand path uses.
type TokenRotator interface {
	RotateTeslaToken(ctx context.Context, userID string) error
}

// KeepaliveConfig tunes the arm. Nested inside Config rather than flattened
// into it so the suspension policy and the credential-hygiene policy stay two
// readable groups instead of one twelve-field struct.
type KeepaliveConfig struct {
	// RefreshAfter is how stale a grant may get before it is rotated.
	RefreshAfter time.Duration
	// RetryAfter is the minimum gap between two attempts at the same owner,
	// whatever came of the first. This is the rotation-loop brake: a rotation
	// whose STORE failed leaves the clock unmoved, and without this gate the
	// next pass would spend another single-use token on the same broken write.
	RetryAfter time.Duration
	// FailureCooldown is how long a refusal suppresses further attempts. Time
	// is the backstop; the store releases the cooldown earlier if anything
	// rotates the grant in the meantime.
	FailureCooldown time.Duration
	// MaxPerSweep caps the rotations one pass performs, and so caps the signed
	// calls this arm can make against Tesla's OAuth endpoint in an hour.
	MaxPerSweep int
	// Timeout bounds one owner's rotation — the Tesla call plus the writes it
	// happens between, all of it holding a row lock.
	Timeout time.Duration
}

const (
	defaultKeepaliveRefreshAfter    = 45 * 24 * time.Hour
	defaultKeepaliveRetryAfter      = 24 * time.Hour
	defaultKeepaliveFailureCooldown = 7 * 24 * time.Hour
	defaultKeepaliveMaxPerSweep     = 20
	defaultKeepaliveTimeout         = 30 * time.Second

	// maxKeepaliveRefreshAfter is a ceiling, not a default. Tesla lapses an
	// unused refresh token at roughly ninety days; a configured value anywhere
	// near that would make the whole arm ornamental, refreshing grants that had
	// already died. Corrected rather than trusted, for the same reason
	// withDefaults corrects an inverted warn/suspend pair.
	maxKeepaliveRefreshAfter = 60 * 24 * time.Hour
)

func (k KeepaliveConfig) withDefaults() KeepaliveConfig {
	if k.RefreshAfter <= 0 {
		k.RefreshAfter = defaultKeepaliveRefreshAfter
	}
	if k.RefreshAfter > maxKeepaliveRefreshAfter {
		k.RefreshAfter = defaultKeepaliveRefreshAfter
	}
	if k.RetryAfter <= 0 {
		k.RetryAfter = defaultKeepaliveRetryAfter
	}
	if k.FailureCooldown <= 0 {
		k.FailureCooldown = defaultKeepaliveFailureCooldown
	}
	if k.MaxPerSweep <= 0 {
		k.MaxPerSweep = defaultKeepaliveMaxPerSweep
	}
	if k.Timeout <= 0 {
		k.Timeout = defaultKeepaliveTimeout
	}
	return k
}

// keepaliveArm bundles the arm's two collaborators so the Sweeper carries one
// field for the whole feature rather than two, and so "is the keepalive wired?"
// is a single nil check.
type keepaliveArm struct {
	store   KeepaliveStore
	rotator TokenRotator
}

// WithKeepalive enables the keepalive arm. Omitted (the default, and every
// deploy without Tesla OAuth credentials) the arm does not exist and no pass
// spends a call on it — the same nil-means-unwired convention the config
// remover and token source already follow.
func WithKeepalive(store KeepaliveStore, rotator TokenRotator) SweeperOption {
	return func(s *Sweeper) {
		if store == nil || rotator == nil {
			// Half a keepalive is not a keepalive: a store with no rotator
			// would list candidates it cannot act on, and a rotator with no
			// store would rotate without ever recording a refusal, which is
			// precisely the hourly-retry-forever failure the record prevents.
			return
		}
		s.keep = &keepaliveArm{store: store, rotator: rotator}
	}
}
