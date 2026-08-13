// The GROUP RIDE's write paths (MYR-540): minting the join code at accept,
// redeeming it, and the two membership probes the ACL widening rests on.
//
// The whole safety argument is that each of these is ONE guarded statement.
// The mint's `join_code IS NULL` is the exactly-once latch; the redeem's
// predicate is the entire refusal set; the membership insert's ON CONFLICT is
// the idempotence. Nothing here checks-then-writes.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// rideJoinCodeMintAttempts bounds the collision retry on the join-code mint.
//
// The space is 36^6 ≈ 2.2 billion and the live set is the handful of group
// rides currently accepted, so a single collision is already implausible and
// three in a row is not a case worth engineering for — it is a case worth
// FAILING for, loudly, rather than looping. Mirrors mintUnusedShareCode's
// bounded-retry shape.
const rideJoinCodeMintAttempts = 3

// MintJoinCode stamps a fresh join code and expiry onto an accepted GROUP ride
// and returns them. Runs on q — the pool for a standalone call, or the open
// accept transaction, which is where it is actually used, so the code and the
// status it depends on commit or roll back together.
//
// IDEMPOTENT AND EXACTLY-ONCE by the statement's own guard: a ride that already
// carries a code, a ride that is not a group ride, and a ride that has reached a
// terminal status all match zero rows and come back as ("", nil, nil) — "there
// is no code to mint here", which every caller treats as an ordinary answer
// rather than an error. That is deliberate: the accept path must not fail an
// otherwise-good transition because the link could not be minted a second time.
//
// The returned code is P1 AND BEARER. It is never logged here and callers must
// not log it either — the only thing that may leave the process is the SIGNED
// URL built from it, on a party-scoped REST surface.
func (r *RideRequestRepo) MintJoinCode(ctx context.Context, q pgxQuerier, rideID string) (string, *time.Time, error) {
	const op = "ride_request.mint_join_code"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	for attempt := 0; attempt < rideJoinCodeMintAttempts; attempt++ {
		code, err := newShareCode()
		if err != nil {
			r.metrics.IncQueryError(op)
			return "", nil, fmt.Errorf("RideRequestRepo.MintJoinCode(%s): %w", rideID, err)
		}

		var (
			minted    string
			expiresAt *time.Time
		)
		err = q.QueryRow(ctx, queryRideRequestMintJoinCode, rideID, code).Scan(&minted, &expiresAt)
		switch {
		case err == nil:
			return minted, expiresAt, nil
		case errors.Is(err, pgx.ErrNoRows):
			// Not an error: the guard refused. Either this ride is not a group
			// ride, or it already has a code, or it is over. All three mean
			// "nothing to mint", and the caller carries on.
			return "", nil, nil
		case isRideJoinCodeCollision(err):
			// uq_go_ride_requests_join_code rejected the draw. Try another.
			continue
		default:
			r.metrics.IncQueryError(op)
			return "", nil, fmt.Errorf("RideRequestRepo.MintJoinCode(%s): %w", rideID, err)
		}
	}
	r.metrics.IncQueryError(op)
	return "", nil, fmt.Errorf("RideRequestRepo.MintJoinCode(%s): %d code collisions in a row", rideID, rideJoinCodeMintAttempts)
}

// isRideJoinCodeCollision reports whether err is the join-code unique index
// rejecting a freshly drawn code. Matched on BOTH the SQLSTATE and the
// constraint name, the house rule for this table: the two other unique indexes
// on go_ride_requests (the per-rider and per-vehicle active-instant guards) are
// entirely different conditions and must never be retried as a code collision.
func isRideJoinCodeCollision(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgUniqueViolation &&
		pgErr.ConstraintName == constraintRideJoinCode
}

// constraintRideJoinCode is the partial UNIQUE index name from migration 0040.
const constraintRideJoinCode = "uq_go_ride_requests_join_code"

// JoinRideByCode redeems a group-ride join code for one account, atomically:
// resolve the code, refuse a caller who is already a party in another role,
// record the membership, and return the FULL ride the caller now belongs to
// with themselves already in Members.
//
// Everything runs in ONE transaction, which is what makes the answer honest.
// Without it, a ride could reach a terminal status between the resolve and the
// insert and leave a membership row on a dead ride — a standing entry in a
// `members` array and, worse, in the access set that admits a WebSocket to that
// ride's vehicle.
//
// `created` reports whether this call inserted the row (false = the same account
// re-redeeming). It changes NOTHING about the response — the contract's
// idempotence promise is that a retry answers 200 with the same ride — and
// exists only so the caller can publish the `ride.member_joined` event once
// rather than on every retry.
//
// Refusals:
//   - ErrRideJoinCodeNotFound — unknown, expired, or terminal. One sentinel, by
//     design; see its doc.
//   - ErrRideJoinSelfParty — the caller is the requester or the vehicle's owner.
func (r *RideRequestRepo) JoinRideByCode(ctx context.Context, code, userID string) (RideRequestRecord, bool, error) {
	const op = "ride_request.join_by_code"
	start := time.Now()
	defer func() { r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds()) }()

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		r.metrics.IncQueryError(op)
		// The code is P1 and is deliberately absent from every error in this
		// function — an error string is exactly the sort of place a bearer
		// credential leaks into a log.
		return RideRequestRecord{}, false, fmt.Errorf("RideRequestRepo.JoinRideByCode: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	rec, err := r.scanRideRequest(tx.QueryRow(ctx, queryRideRequestByJoinCode, code))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RideRequestRecord{}, false, fmt.Errorf("RideRequestRepo.JoinRideByCode: %w", ErrRideJoinCodeNotFound)
		}
		r.metrics.IncQueryError(op)
		return RideRequestRecord{}, false, fmt.Errorf("RideRequestRepo.JoinRideByCode: %w", err)
	}

	// The requester is on the ride by having booked it and the owner is a party
	// without riding in it. Neither may become a member — see
	// ErrRideJoinSelfParty for why this is a 409 and not a silent success.
	if userID == rec.RiderID || userID == rec.OwnerID {
		return RideRequestRecord{}, false, fmt.Errorf("RideRequestRepo.JoinRideByCode(%s): %w", rec.ID, ErrRideJoinSelfParty)
	}

	created := true
	var joinedAt time.Time
	if err := tx.QueryRow(ctx, queryInsertRideMember, rec.ID, userID).Scan(&joinedAt); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			r.metrics.IncQueryError(op)
			return RideRequestRecord{}, false, fmt.Errorf("RideRequestRepo.JoinRideByCode(%s): insert member: %w", rec.ID, err)
		}
		// ON CONFLICT DO NOTHING returned nothing: this account is already a
		// member. The contract's idempotence — same 200, same ride, no second
		// row — is exactly this branch.
		created = false
	}

	// Read the lists back INSIDE the transaction, on the same querier, so the
	// body the joiner receives already contains themselves. A read after commit
	// could race a concurrent join and answer with a list that does not include
	// the caller — the one thing this response has to get right.
	rec, err = r.withGroup(ctx, tx, rec)
	if err != nil {
		r.metrics.IncQueryError(op)
		return RideRequestRecord{}, false, fmt.Errorf("RideRequestRepo.JoinRideByCode(%s): %w", rec.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		r.metrics.IncQueryError(op)
		return RideRequestRecord{}, false, fmt.Errorf("RideRequestRepo.JoinRideByCode(%s): commit: %w", rec.ID, err)
	}
	return rec, created, nil
}

// IsRideMember reports whether userID has joined rideID. It is the ACL probe
// every party-scoped ride surface widens through, so it is deliberately a plain
// indexed EXISTS and nothing else — no status filter.
//
// WHY NO STATUS FILTER, when the ACCESS-SET query has one. They answer different
// questions. This one is "is this person a party to this ride", which stays true
// after the ride ends, exactly as it stays true for the requester and the owner:
// a member who rode yesterday can still read yesterday's ride, and the read
// paths' own terminal-status rules are what stop them doing anything to it. The
// access-set query is "may this person see live telemetry from this car", which
// must end when the ride does.
func (r *RideRequestRepo) IsRideMember(ctx context.Context, rideID, userID string) (bool, error) {
	if rideID == "" || userID == "" {
		return false, nil
	}
	const op = "ride_request.is_member"
	start := time.Now()
	var exists bool
	err := r.pool.QueryRow(ctx, queryRideMemberExists, rideID, userID).Scan(&exists)
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError(op)
		return false, fmt.Errorf("RideRequestRepo.IsRideMember(%s): %w", rideID, err)
	}
	return exists, nil
}
