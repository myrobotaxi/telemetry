package ws

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// DefaultRevalidateInterval is how often the backstop sweep re-derives every
// connected user's access set. It is a BACKSTOP, not the mechanism: the
// event-driven nudge (hub_access.go RevokeUserAccess, driven by the share
// mutation path) is what actually makes a revocation take effect, and it does
// so in milliseconds. This sweep exists because that nudge is not a delivery
// guarantee.
//
// Specifically, the in-process bus drops the OLDEST event when a subscriber's
// buffer is full (internal/events/subscriber.go), the publishing handler and
// the hub can only agree in-process so a mutation served by a different
// machine reaches this hub's sockets through nothing at all, and any future
// mutation path that forgets to publish would silently reopen DV-09. A
// security property that depends on every caller remembering to announce it is
// not a security property. This sweep re-derives from the database and is
// therefore correct regardless of who mutated what, where.
const DefaultRevalidateInterval = 60 * time.Second

// AccessResolver re-derives a user's authorized vehicle set. Defined at the
// consumer site; satisfied by the same *auth.JWTAuthenticator the handshake
// already uses, so the sweep and the handshake cannot disagree about what a
// user may see.
//
// Note the cache underneath: JWTAuthenticator serves this from a 5-minute
// per-process cache. On the machine that served the mutation the entry was
// busted synchronously, so the sweep reads fresh and the backstop bound is one
// interval. On any OTHER machine the entry lapses on its own TTL, so the bound
// there is the TTL plus one interval. Both are bounded; only the first is fast.
type AccessResolver interface {
	GetUserVehicles(ctx context.Context, userID string) ([]string, error)
}

// AccessRevalidator periodically re-derives every connected user's access set
// and closes sessions that are holding a vehicle the user can no longer see
// (MYR-373, websocket-protocol.md §10 DV-09).
type AccessRevalidator struct {
	hub      *Hub
	resolver AccessResolver
	interval time.Duration
	logger   *slog.Logger
}

// NewAccessRevalidator builds the backstop sweep. A non-positive interval
// falls back to DefaultRevalidateInterval.
func NewAccessRevalidator(hub *Hub, resolver AccessResolver, interval time.Duration, logger *slog.Logger) *AccessRevalidator {
	if interval <= 0 {
		interval = DefaultRevalidateInterval
	}
	return &AccessRevalidator{hub: hub, resolver: resolver, interval: interval, logger: logger}
}

// Run sweeps every interval until ctx is cancelled. Intended to be started as
// a goroutine at wiring time. It does NOT sweep immediately on start: at
// startup there are no connections to revalidate, and a sweep racing the first
// handshakes buys nothing.
func (r *AccessRevalidator) Run(ctx context.Context) {
	if r.hub == nil || r.resolver == nil {
		return
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.SweepOnce(ctx)
		}
	}
}

// SweepOnce runs a single revalidation pass and returns the number of sessions
// it closed. Exported so a test can drive one deterministic pass instead of
// waiting on the ticker.
func (r *AccessRevalidator) SweepOnce(ctx context.Context) int {
	clients := r.hub.snapshotClients()
	if len(clients) == 0 {
		return 0
	}

	// One resolver call per distinct USER, not per connection: a user with
	// three tabs open is one access set, and the handshake cache would serve
	// the other two from memory anyway. Resolving once also guarantees every
	// session of that user is judged against the SAME answer, so a sweep
	// cannot close one tab and spare another on a cache expiry landing
	// mid-loop.
	resolved := make(map[string]accessSet, len(clients))
	closed := 0

	for _, client := range clients {
		// Dev-mode wildcard clients are authorized for everything by
		// construction; there is no set to narrow.
		if client.allVehicles || client.userID == "" {
			continue
		}

		allowed, ok := resolved[client.userID]
		if !ok {
			var err error
			allowed, err = r.resolve(ctx, client.userID)
			if err != nil {
				// FAIL OPEN, DELIBERATELY. A database blip must not
				// disconnect every connected client at once; the
				// event-driven nudge is the mechanism and this is the
				// backstop, so a skipped pass costs one interval of
				// staleness while a fail-closed sweep would turn a
				// transient query error into a fleet-wide outage.
				r.logger.Warn("access revalidation skipped: resolve failed",
					slog.String("user_id", client.userID),
					slog.Any("error", err),
				)
				continue
			}
			resolved[client.userID] = allowed
		}

		if lost, found := firstLostVehicle(client, allowed); found {
			closed += r.hub.RevokeUserAccess(client.userID, lost, "revalidation_backstop")
		}
	}
	return closed
}

// accessSet is one user's current entitlement.
//
// The `all` flag is why this is a struct and not a bare map. "All access" and
// "no access" both produce zero per-vehicle entries, and they are opposite
// answers: the first must close nothing, the second must close everything that
// user has open. A bare map would have to encode the difference as nil-versus-
// empty, which is exactly the distinction a future edit drops.
type accessSet struct {
	// all is the dev-mode wildcard sentinel: authorized for every vehicle.
	all bool
	// allowed holds the concrete vehicle ids. Empty and non-nil is a real
	// answer — the user may see nothing.
	allowed map[string]struct{}
}

// resolve fetches the user's current entitlement.
func (r *AccessRevalidator) resolve(ctx context.Context, userID string) (accessSet, error) {
	ids, err := r.resolver.GetUserVehicles(ctx, userID)
	if err != nil {
		return accessSet{}, fmt.Errorf("revalidator.resolve(user=%s): %w", userID, err)
	}
	out := accessSet{allowed: make(map[string]struct{}, len(ids))}
	for _, id := range ids {
		if id == WildcardVehicleID {
			return accessSet{all: true}, nil
		}
		out.allowed[id] = struct{}{}
	}
	return out, nil
}

// firstLostVehicle reports a vehicle the client is still holding that the
// user's current entitlement no longer covers. One is enough: RevokeUserAccess
// closes the whole session, so finding a second would change nothing.
func firstLostVehicle(c *Client, set accessSet) (string, bool) {
	if set.all {
		return "", false
	}
	for _, held := range c.authorizedVehicles() {
		if _, ok := set.allowed[held]; !ok {
			return held, true
		}
	}
	return "", false
}
