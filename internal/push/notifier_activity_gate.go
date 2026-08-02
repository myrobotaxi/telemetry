package push

import (
	"context"
	"log/slog"
)

// The duplicate-banner gate (MYR-413).
//
// THE FIELD REPORT. At `arrived` the rider's phone showed the compact Dynamic
// Island with a lock-screen banner sitting on top of it — "Your car is here —
// your turn to start" — and the client's reading of that screenshot was "instead
// of expanding the dynamic island you're pushing push notifications". Both
// pushes are ours and they are sent by two different subscribers to one event:
// the Live Activity notifier attaches an `aps.alert` that asks iOS to expand the
// island (activity_alert.go), and the ride-lifecycle notifier in this package
// sends a separate, ordinary alert push carrying the same news in prose.
//
// They are not merely redundant, they COMPETE. A banner and the island's
// expanded presentation want the same strip of screen, and the banner is the one
// that wins it, so the very push that duplicates the island's message is also
// the thing standing in front of it. Suppressing it is not tidying — it is how
// the expansion becomes visible.
//
// THE RULE, and its exact edge. A lifecycle banner is skipped only when all of:
//
//   - the transition is one the island alerts on (see alertLadderStatuses), and
//   - the RECIPIENT has a live, non-tombstoned Activity on that same ride.
//
// Everything else keeps its banner, and each exclusion is load-bearing rather
// than cautious:
//
//   - `declined` is outside the design's six phases, so its Activity update
//     carries no alert and expands nothing. Suppressing that banner would leave
//     the worst news of the ride to a card the rider may not be looking at, so
//     it sends unconditionally. `cancelled` and `reservation_expired` need no
//     exclusion for a different reason: statusAlert (copy.go) generates a
//     lifecycle banner for `accepted`, `declined` and `arrived` only, so those
//     two have never produced one and there is nothing here to suppress.
//   - A TOMBSTONED or DELETED row reads as no row (store.HasLiveActivity), so a
//     rider who swiped the card away, or whose token APNs rejected, keeps every
//     banner. A swiped-away card must never leave somebody dark.
//   - The OWNER'S pushes are untouched, and this falls out of the key rather
//     than needing a rule: the row is keyed (ride, user) and v1 registers only
//     the rider, so an owner's lookup finds nothing and their banner is sent.
//
// WHY THE `ride.request.created` PUSH IS NOT GATED, even though `requested` is
// on the ladder. That push goes to the OWNER and asks them to accept or decline;
// it is a different message to a different person, not a phase announcement. The
// distinction is not academic on this platform — an owner who rides their own
// car is both parties, so gating the created push would let the rider's card
// swallow the owner's approval prompt and the ride would never be accepted at
// all.
//
// WHY `ride.due` IS NOT GATED. The reservation sweeper's "your car is heading
// your way" rides its own topic and moves no ride status, so the ladder never
// advances and the island never expands for it. A gate there would delete the
// notification rather than defer to a better one.

// ActivityPresenceStore answers whether one party is currently watching a Live
// Activity for one ride.
//
// Declared here at the consumer site, like DeviceStore and PrefStore beside it,
// so internal/push keeps its independence from internal/store. Satisfied by
// *store.LiveActivityRepo directly — the signature is the repository method's,
// so the cmd wiring needs no adapter.
type ActivityPresenceStore interface {
	HasLiveActivity(ctx context.Context, rideRequestID, userID string) (bool, error)
}

// notifierStores groups the three persistence seams the fan-out consults. They
// are bundled rather than sitting side by side on Notifier because they are
// asked the same question about the same person on the same code path — "may I
// wake them, where, and would it be a duplicate?" — and because the struct was
// already at the field count this project's rules call a God struct.
type notifierStores struct {
	devices    DeviceStore
	prefs      PrefStore
	activities ActivityPresenceStore
}

// alertLadderStatuses is the set of ride statuses whose Live Activity update
// carries an island-expanding `aps.alert`.
//
// It is the STATUS half of activity_alert.go's six-rung ladder, and the two
// files are pinned together by a test rather than by a shared constant, because
// they answer subtly different questions. `alertPhaseFor` maps a ride's state
// onto a rung and needs the car's ETA to tell Enroute from Arriving; this map
// only needs to know that `accepted` alerts at all, which is true either way.
// Arriving is absent because it is not a ride status — it is a threshold the ETA
// ticker crosses, so no lifecycle banner is ever sent for it and there is
// nothing here to suppress.
//
// `declined` is deliberately absent, not forgotten: it is outside the design's
// six, its Activity update expands no island, and its banner must therefore
// survive. `cancelled` and `reservation_expired` are absent for a weaker reason
// — statusAlert produces no banner for them at all, so their absence here
// changes nothing either way.
//
// `requested`, `enroute` and `completed` are carried for forward-safety against
// the ladder, not because a banner exists for them today — `statusAlert` sends
// nothing on those transitions, so the gate is currently reachable only on
// `accepted` and `arrived`.
var alertLadderStatuses = map[string]bool{
	statusRequested: true,
	statusAccepted:  true,
	statusArrived:   true,
	statusEnroute:   true,
	statusCompleted: true,
}

// carriesIslandAlert reports whether a transition to `status` is one the rider's
// Live Activity announces by expanding the island.
func carriesIslandAlert(status string) bool { return alertLadderStatuses[status] }

// duplicatesLiveActivity reports whether this fan-out would put a banner in
// front of an island that is already telling the recipient the same thing.
//
// IT FAILS OPEN in all three of its failure modes — no store wired, a lookup
// error, and a delivery that was never marked suppressible — for the same reason
// the preference gate next door does, and the direction matters more here than
// there. This package's standing rule is that a duplicate notification is a
// minor annoyance to a human whereas a missed one is a rider standing on a
// sidewalk; a gate that failed CLOSED would turn a transient database hiccup
// into a rider who is told nothing, by either surface, that their car arrived.
func (n *Notifier) duplicatesLiveActivity(ctx context.Context, d delivery) bool {
	if !d.islandAlerts || n.stores.activities == nil || d.rideID == "" {
		return false
	}

	live, err := n.stores.activities.HasLiveActivity(ctx, d.rideID, d.userID)
	if err != nil {
		n.logger.Error("push: live activity lookup failed; sending the banner anyway",
			slog.String("topic", d.topic),
			slog.String("ride_id", d.rideID),
			slog.String("user_id", d.userID),
			slog.String("error", err.Error()),
		)
		return false
	}
	if !live {
		return false
	}

	// P0 throughout — two opaque cuids and a topic. Logged at Info rather than
	// Debug for the same reason the preference gate is: "the notification I
	// expected never arrived" is a support question, and this line is the whole
	// answer to it.
	n.logger.Info("push suppressed by live activity",
		slog.String("topic", d.topic),
		slog.String("ride_id", d.rideID),
		slog.String("user_id", d.userID),
	)
	return true
}
