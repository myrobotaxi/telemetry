// The GROUP RIDE's member list (MYR-540) — the READ path.
//
// A member is somebody who redeemed the ride's shared link. The requester is
// not one of them (they are go_ride_requests.rider_id) and neither is the
// owner, which is what keeps every pre-MYR-540 row byte-identical on the wire:
// absent means "no joiners", full stop.
//
// WHY THE READ IS A SECOND QUERY — the MYR-539 argument, unchanged.
// rideRequestColumns is already the widest projection in this repo (four
// AES-256-GCM decrypts and three identity subselects per row) and it is on
// every ride read there is, including the background probes that deliberately
// do NOT use it. Members are ROWS, so folding them in as a join would multiply
// each ride row by its member count and make the lean reads' argument untrue by
// construction. The surfaces that serve §7.8's RideRequest object therefore
// attach members with ONE extra statement — `ride_id = ANY($1)` for a whole
// page, not one query per row — and every other read stays as lean as it was.
//
// Classification: the member's user id is P0, the derived first name is P1 PII
// (data-classification.md §1.9's first-names-only policy, the same tier as
// requesterName). Nothing in this file logs either.

package store

import (
	"context"
	"fmt"
	"time"
)

// memberIdentitySelect resolves a member's display identity INLINE, in the same
// statement as the membership row, through the SAME three sources and the SAME
// precedence requesterIdentitySelect uses (MYR-264): the Prisma-owned "User"
// first, then the Apple first-consent name, then go_users. NULLIF drops empty
// strings so COALESCE falls through to the next non-empty source.
//
// All three are READ-ONLY (CG-DL-9). Unlike requesterIdentitySelect there is no
// `exists` probe: RideMember.firstName is REQUIRED by the contract, so a member
// with no identity row anywhere still has to print something, and the ladder's
// own "Rider" fallback is that something. A missing identity NEVER fails the
// surrounding read.
const memberIdentitySelect = `
	COALESCE(
		NULLIF((SELECT u."name" FROM "User" u WHERE u."id" = m.user_id), ''),
		NULLIF((SELECT a."name" FROM go_identity_apple a WHERE a.user_id = m.user_id ORDER BY a.last_login_at DESC LIMIT 1), ''),
		NULLIF((SELECT g."name" FROM go_users g WHERE g.id = m.user_id), '')
	) AS member_name,
	COALESCE(
		NULLIF((SELECT u."email" FROM "User" u WHERE u."id" = m.user_id), ''),
		NULLIF((SELECT a."email" FROM go_identity_apple a WHERE a.user_id = m.user_id ORDER BY a.last_login_at DESC LIMIT 1), ''),
		NULLIF((SELECT g."email" FROM go_users g WHERE g.id = m.user_id), '')
	) AS member_email`

// queryRideMembersByRides loads the members of one OR MANY rides in JOIN ORDER,
// which is the "server-defined order" the contract names (ascending join time).
// `ride_id = ANY($1)` is what keeps the list surfaces at one statement per page
// instead of one per row; the (ride_id, user_id) primary key serves the filter.
//
// The user_id tie-break makes the order TOTAL: two members who joined inside the
// same clock tick would otherwise come back in an arbitrary order that could
// change between two reads of an unchanged ride, which a client diffing the list
// would render as churn.
const queryRideMembersByRides = `SELECT m.ride_id, m.user_id, m.joined_at,` + memberIdentitySelect + `
FROM go_ride_members m
WHERE m.ride_id = ANY($1)
ORDER BY m.ride_id, m.joined_at, m.user_id`

// loadMembers returns the members of every requested ride, keyed by ride id and
// in join order. Rides with no members are simply absent from the map — the
// contract reads absence as "no joiners", never as "unknown".
func (r *RideRequestRepo) loadMembers(ctx context.Context, q pgxQuerierRows, rideIDs []string) (map[string][]RideMember, error) {
	out := make(map[string][]RideMember)
	if len(rideIDs) == 0 {
		return out, nil
	}
	const op = "ride_request.load_members"
	start := time.Now()
	rows, err := q.Query(ctx, queryRideMembersByRides, rideIDs)
	if err != nil {
		r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("load ride members: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			rideID string
			member RideMember
			name   *string
			email  *string
		)
		if err := rows.Scan(&rideID, &member.UserID, &member.JoinedAt, &name, &email); err != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("load ride members: scan: %w", err)
		}
		// The same ladder requesterName takes, and therefore the same guarantee:
		// a non-empty string on every row, so no consumer needs an absent-name
		// branch.
		member.FirstName = requesterDisplayName(name, email)
		out[rideID] = append(out[rideID], member)
	}
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("load ride members: rows: %w", err)
	}
	return out, nil
}

// attachMembers fills in the Members field of every record in place — ONE extra
// statement for the whole slice. A ride with no members keeps a nil slice, which
// is the wire's omitted key.
//
// Like attachStops it is called at the FUNNELS that serve the wire RideRequest
// object, never inside scanRideRequest: the lean background reads share the
// scanner but must not acquire its cost.
//
// IT SKIPS THE STATEMENT ENTIRELY WHEN NO RECORD IS A GROUP RIDE, which is the
// overwhelmingly common page. Membership is impossible without groupRide (the
// join code is minted only for one), so the filter cannot hide a member — and
// it keeps the cost of this feature at zero for every solo-ride list, which is
// the same bargain the stops read makes by returning an empty result set.
func (r *RideRequestRepo) attachMembers(ctx context.Context, q pgxQuerierRows, recs []RideRequestRecord) error {
	ids := make([]string, 0, len(recs))
	for i := range recs {
		if recs[i].GroupRide {
			ids = append(ids, recs[i].ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	byRide, err := r.loadMembers(ctx, q, ids)
	if err != nil {
		return err
	}
	if len(byRide) == 0 {
		return nil
	}
	for i := range recs {
		if members, ok := byRide[recs[i].ID]; ok {
			recs[i].Members = members
		}
	}
	return nil
}

// withMembers is attachMembers for a single record, returned by value so the
// single-row read paths stay one-liners.
func (r *RideRequestRepo) withMembers(ctx context.Context, q pgxQuerierRows, rec RideRequestRecord) (RideRequestRecord, error) {
	recs := []RideRequestRecord{rec}
	if err := r.attachMembers(ctx, q, recs); err != nil {
		return RideRequestRecord{}, err
	}
	return recs[0], nil
}

// withGroup attaches BOTH row-set projections a §7.8 RideRequest carries — the
// MYR-539 stops and the MYR-540 members. Every funnel that serves the wire
// object calls this one helper rather than the two in sequence, so a new
// surface cannot pick up one and forget the other.
func (r *RideRequestRepo) withGroup(ctx context.Context, q pgxQuerierRows, rec RideRequestRecord) (RideRequestRecord, error) {
	rec, err := r.withStops(ctx, q, rec)
	if err != nil {
		return RideRequestRecord{}, err
	}
	return r.withMembers(ctx, q, rec)
}
