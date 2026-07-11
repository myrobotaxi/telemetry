// Package store — RideRequestRepo (MYR-173) persists the P10 ride-hailing
// aggregate in the Go-owned go_ride_requests table.
//
// Encryption contract (NFR-3.23, data-classification.md §1.9): pickup and
// dropoff coordinates are P1 GPS data stored encrypt-only — the table has
// no plaintext coordinate columns, so unlike the Vehicle GPS dual-write
// (MYR-63) there is no fallback path and the Encryptor is mandatory at
// construction. Encrypt failures fail the write (a ride request without a
// pickup is useless, unlike telemetry where fail-open protects the
// stream); decrypt failures fail the read.
//
// No HTTP surface here — MYR-174 (rider API) and MYR-175 (owner
// accept/decline) bind handlers onto these accessors.

package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// pgUniqueViolation is the SQLSTATE Postgres raises when an INSERT/UPDATE
// violates a unique constraint (unique_violation).
const pgUniqueViolation = "23505"

// isRideActiveViolation reports whether err is the partial-unique-index
// rejection from migration 0004 — the rider already holds an OPEN instant
// ride. Matches on BOTH the SQLSTATE and the constraint name so an unrelated
// future unique constraint on the table is not misclassified as an
// active-ride conflict.
func isRideActiveViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgUniqueViolation &&
		pgErr.ConstraintName == constraintRideActiveInstant
}

// RideRequestRepo manages ride-request records. All coordinate columns are
// AES-256-GCM ciphertext; the repo is the encrypt/decrypt boundary
// (NFR-3.25 — callers only ever see plaintext RidePlace values).
type RideRequestRepo struct {
	pool      *pgxpool.Pool
	metrics   Metrics
	encryptor cryptox.Encryptor
}

// NewRideRequestRepo constructs the repo. The Encryptor is required —
// go_ride_requests stores GPS coordinates encrypt-only, so a nil Encryptor
// can neither write nor read a single row. Panics on nil to fail loud at
// construction (mirrors NewDriveRepoWithEncryption).
func NewRideRequestRepo(pool *pgxpool.Pool, metrics Metrics, encryptor cryptox.Encryptor) *RideRequestRepo {
	if encryptor == nil {
		panic("store.NewRideRequestRepo: encryptor must not be nil")
	}
	return &RideRequestRepo{pool: pool, metrics: metrics, encryptor: encryptor}
}

// rideRequestIDRandomBytes sizes the random suffix of a generated ride
// request id: 16 bytes -> 32 hex chars + the "c" prefix, within the cuid
// length envelope used by the platform's other tables (see
// internal/mask/audit.go newAuditID for the convention).
const rideRequestIDRandomBytes = 16

// newRideRequestID generates a cuid-shaped identifier for a new row.
func newRideRequestID() string {
	b := make([]byte, rideRequestIDRandomBytes)
	if _, err := rand.Read(b); err != nil {
		// OS RNG unavailable is unrecoverable; a timestamp-derived id keeps
		// the request usable rather than panicking mid-request.
		return fmt.Sprintf("c%x", time.Now().UnixNano())
	}
	return "c" + hex.EncodeToString(b)
}

// Create inserts a new ride request. Zero-value fields get their creation
// defaults: an empty ID is generated, an empty Status becomes 'requested'.
// AcceptedAt/CompletedAt/Reschedule* are ignored on insert — they only
// come into existence through UpdateStatus / ProposeReschedule. Returns
// the stored record with DB-assigned created_at/updated_at.
func (r *RideRequestRepo) Create(ctx context.Context, req RideRequestRecord) (RideRequestRecord, error) {
	if req.ID == "" {
		req.ID = newRideRequestID()
	}
	if req.Status == "" {
		req.Status = RideRequestStatusRequested
	}

	// Resolve the requester's display name (MYR-229) BEFORE the INSERT: the
	// rider id is already known, and resolving first means a "User" lookup
	// failure fails the create cleanly instead of orphaning a committed row
	// whose caller then sees a 500.
	requesterName, err := r.requesterName(ctx, req.RiderID)
	if err != nil {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.Create(%s): %w", req.ID, err)
	}

	pickupLatEnc, pickupLngEnc, err := r.encryptPlace(req.Pickup)
	if err != nil {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.Create(%s): pickup: %w", req.ID, err)
	}
	dropoffLatEnc, dropoffLngEnc, err := r.encryptPlace(req.Dropoff)
	if err != nil {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.Create(%s): dropoff: %w", req.ID, err)
	}

	start := time.Now()
	row := r.pool.QueryRow(ctx, queryRideRequestInsert,
		req.ID, req.RiderID, req.OwnerID, req.VehicleID,
		pickupLatEnc, pickupLngEnc, req.Pickup.Label, req.Pickup.Address,
		dropoffLatEnc, dropoffLngEnc, req.Dropoff.Label, req.Dropoff.Address,
		string(req.Status), req.PassengerName, req.PassengerPhone, req.ScheduledFor,
	)
	err = row.Scan(&req.CreatedAt, &req.UpdatedAt)
	r.metrics.ObserveQueryDuration("ride_request.create", time.Since(start).Seconds())
	if err != nil {
		// The one-active-instant-ride guard (0004) losing the race is an
		// expected outcome, not a query fault — surface the typed sentinel
		// without incrementing the error counter (mirrors the guarded
		// UpdateStatusFrom conflict path).
		if isRideActiveViolation(err) {
			return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.Create(rider %s): %w", req.RiderID, ErrRideRequestActive)
		}
		r.metrics.IncQueryError("ride_request.create")
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.Create(%s): %w", req.ID, err)
	}
	req.AcceptedAt, req.CompletedAt = nil, nil
	req.RescheduleProposedFor, req.RescheduleStatus = nil, nil
	req.RequesterName = requesterName
	return req, nil
}

// GetByID returns a single ride request. Returns ErrRideRequestNotFound
// when no row has that id.
func (r *RideRequestRepo) GetByID(ctx context.Context, id string) (RideRequestRecord, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryRideRequestByID, id)
	rec, err := r.scanRideRequest(row)
	r.metrics.ObserveQueryDuration("ride_request.get_by_id", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.GetByID(%s): %w", id, ErrRideRequestNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("ride_request.get_by_id")
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.GetByID(%s): %w", id, err)
	}
	if err := r.attachRequesterName(ctx, &rec); err != nil {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.GetByID(%s): %w", id, err)
	}
	return rec, nil
}

// GetActiveInstantByRider returns the rider's single OPEN instant ride
// request — the one row the partial unique index (0004) permits. OPEN = the
// non-terminal lifecycle states; instant = no scheduledFor. Used by the
// create handler to populate the 409 `ride_active` body so the client can
// adopt the existing ride (MYR-230). Returns ErrRideRequestNotFound when the
// rider has no open instant ride.
func (r *RideRequestRepo) GetActiveInstantByRider(ctx context.Context, riderID string) (RideRequestRecord, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryRideRequestActiveInstantByRider, riderID)
	rec, err := r.scanRideRequest(row)
	r.metrics.ObserveQueryDuration("ride_request.get_active_instant_by_rider", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.GetActiveInstantByRider(%s): %w", riderID, ErrRideRequestNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("ride_request.get_active_instant_by_rider")
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.GetActiveInstantByRider(%s): %w", riderID, err)
	}
	if err := r.attachRequesterName(ctx, &rec); err != nil {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.GetActiveInstantByRider(%s): %w", riderID, err)
	}
	return rec, nil
}

// UpdateStatus persists a lifecycle transition with its timestamp
// side-effects: entering 'accepted' stamps AcceptedAt, entering
// 'completed' stamps CompletedAt (first entry only — an already-set stamp
// never moves), and every transition touches UpdatedAt. Transition
// legality (who may move a ride to which state) is the callers' concern —
// MYR-175 owner accept/decline and MYR-176 dispatch enforce it; the store
// stays a dumb persistence layer. Returns the post-update record, or
// ErrRideRequestNotFound.
func (r *RideRequestRepo) UpdateStatus(ctx context.Context, id string, status RideRequestStatus) (RideRequestRecord, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryRideRequestUpdateStatus, id, string(status))
	rec, err := r.scanRideRequest(row)
	r.metrics.ObserveQueryDuration("ride_request.update_status", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateStatus(%s): %w", id, ErrRideRequestNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("ride_request.update_status")
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateStatus(%s): %w", id, err)
	}
	if err := r.attachRequesterName(ctx, &rec); err != nil {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateStatus(%s): %w", id, err)
	}
	return rec, nil
}

// UpdateStatusFrom is the GUARDED transition (MYR-174/175 race fix): it
// persists the same stamped status change as UpdateStatus, but only when
// the row's CURRENT status is in `from` — a single-statement
// `WHERE id = $1 AND status = ANY($3)` so concurrent conflicting mutations
// serialize in the database and exactly one wins. The HTTP handlers use
// this for every lifecycle transition; only the winning call may publish
// the WS / dispatch events (exactly-once ride.accepted seam).
//
// On a zero-row match the miss is disambiguated with a follow-up read (the
// guard's correctness does not depend on it): an unknown id returns
// ErrRideRequestNotFound, an existing row whose status is outside `from`
// returns ErrRideRequestConflict.
func (r *RideRequestRepo) UpdateStatusFrom(ctx context.Context, id string, from []RideRequestStatus, to RideRequestStatus) (RideRequestRecord, error) {
	fromStrs := make([]string, 0, len(from))
	for _, s := range from {
		fromStrs = append(fromStrs, string(s))
	}

	start := time.Now()
	row := r.pool.QueryRow(ctx, queryRideRequestUpdateStatusFrom, id, string(to), fromStrs)
	rec, err := r.scanRideRequest(row)
	r.metrics.ObserveQueryDuration("ride_request.update_status_from", time.Since(start).Seconds())
	if err == nil {
		if nameErr := r.attachRequesterName(ctx, &rec); nameErr != nil {
			return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateStatusFrom(%s): %w", id, nameErr)
		}
		return rec, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		r.metrics.IncQueryError("ride_request.update_status_from")
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateStatusFrom(%s): %w", id, err)
	}

	// Zero rows: not-found vs status-conflict. The follow-up read is purely
	// diagnostic — whatever it observes, the guarded write above already
	// refused the transition.
	if _, getErr := r.GetByID(ctx, id); getErr != nil {
		if errors.Is(getErr, ErrRideRequestNotFound) {
			return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateStatusFrom(%s): %w", id, ErrRideRequestNotFound)
		}
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateStatusFrom(%s): disambiguate miss: %w", id, getErr)
	}
	return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.UpdateStatusFrom(%s -> %s): %w", id, to, ErrRideRequestConflict)
}
