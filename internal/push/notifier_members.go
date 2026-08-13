// GROUP-RIDE fan-out (MYR-540): every ride-lifecycle push a RIDER receives, the
// people riding along with them receive too.
//
// WHY THE MEMBER LIST IS RESOLVED AT DELIVERY TIME rather than carried on the
// event. Every publisher of every ride event would otherwise have to remember to
// populate it, from whatever read it happened to have — and two of them (the
// reservation sweeper's `ride.due`, the dispatcher's seams) work from
// deliberately LEAN rows that carry no member list and must not start carrying
// one. Resolving here is the same shape the vehicle nickname and the requester's
// first name already take: the events stay summary-only, and one lookup on the
// worker covers every topic at once.
//
// THE MEMBER GETS THE RIDER-FLAVOURED COPY, verbatim — the same alert, the same
// category, the same island-alert marking. A member is a passenger: "Your ride
// is confirmed" and "Your car is here" are as true for them as for the person
// who booked it, and forking the copy would mean inventing a second voice for
// the same fact.
//
// NOBODY IS TOLD ABOUT A JOIN. `ride.member_joined` has no push consumer, by the
// client's decision: a notification per joiner would be a group chat's worth of
// banners, and the requester finds out by looking at the ride.

package push

import (
	"context"
	"log/slog"
)

// RideMemberLister resolves a ride to the user ids of its JOINED group members
// (MYR-540). Declared here at the consumer site so internal/push never imports
// internal/store; the cmd/ wiring adapts the repo onto it.
//
// It returns ids only — never names. The notifier addresses phones, and a name
// on this path would be P1 PII travelling for no reader.
type RideMemberLister interface {
	RideMemberIDs(ctx context.Context, rideRequestID string) ([]string, error)
}

// WithRideMembers wires the group-ride fan-out. Optional, following the
// WithRequesterNames precedent: nil (never calling this) is the ordinary
// unwired state, in which every fan-out reaches the two standing parties
// exactly as it did before MYR-540.
func (n *Notifier) WithRideMembers(m RideMemberLister) *Notifier {
	n.members = m
	return n
}

// memberIDs resolves the ride's joined members, EXCLUDING the ids already being
// notified through another role.
//
// The exclusion is what keeps a self-ride and a re-joined party from getting two
// copies of one sentence. It is cheap and it is defensive: the join endpoint
// already refuses the requester and the owner with a 409, so `exclude` should
// never match — but "should never" is not a property a fan-out should rely on
// when the cost of being wrong is a rider's phone buzzing twice.
//
// FAILS QUIET. An unresolvable member list logs and returns nothing, so the
// requester's and owner's pushes still go out. The alternative — abandoning the
// whole fan-out because one lookup failed — would trade a missed member
// notification for a missed RIDER notification, which is the one this system
// exists to deliver.
func (n *Notifier) memberIDs(ctx context.Context, rideID string, exclude ...string) []string {
	if n.members == nil || rideID == "" {
		return nil
	}
	ids, err := n.members.RideMemberIDs(ctx, rideID)
	if err != nil {
		n.logger.Warn("push: ride member lookup failed",
			slog.String("ride_request_id", rideID),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if len(ids) == 0 {
		return nil
	}
	out := ids[:0:0]
	for _, id := range ids {
		if id == "" || containsID(exclude, id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// fanOutMembers delivers one alert to every member of the ride, reusing the
// delivery envelope built for the rider so the copy, the category and the
// MYR-413 island-alert marking cannot drift between the two audiences.
//
// SEQUENTIAL, inside the worker that is already running, sharing the fan-out's
// one timeout budget. That is deliberate: a group is a handful of people, the
// per-recipient work is a registry read plus an APNs round trip, and spawning a
// goroutine per member would put an unbounded multiplier on the very semaphore
// cfg.MaxConcurrent exists to hold. The bound worth having is on APNs traffic,
// and this keeps it.
func (n *Notifier) fanOutMembers(ctx context.Context, base delivery, memberIDs []string, a alert) {
	for _, memberID := range memberIDs {
		d := base
		d.userID = memberID
		n.fanOut(ctx, d, a)
	}
}

// containsID reports membership in a small id slice.
func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
