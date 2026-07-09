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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

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
		r.metrics.IncQueryError("ride_request.create")
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.Create(%s): %w", req.ID, err)
	}
	req.AcceptedAt, req.CompletedAt = nil, nil
	req.RescheduleProposedFor, req.RescheduleStatus = nil, nil
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

// ProposeReschedule records the rider's proposed new pickup time and opens
// the reschedule negotiation (RescheduleStatus 'requested' — the owner is
// asked to re-confirm; MYR-192). The main Status is untouched: the design
// keeps the reservation alive while the ask is pending. Returns the
// post-update record, or ErrRideRequestNotFound.
func (r *RideRequestRepo) ProposeReschedule(ctx context.Context, id string, proposedFor time.Time) (RideRequestRecord, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryRideRequestProposeReschedule, id, proposedFor)
	rec, err := r.scanRideRequest(row)
	r.metrics.ObserveQueryDuration("ride_request.propose_reschedule", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.ProposeReschedule(%s): %w", id, ErrRideRequestNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("ride_request.propose_reschedule")
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.ProposeReschedule(%s): %w", id, err)
	}
	return rec, nil
}

// ResolveReschedule closes an open reschedule negotiation. confirmed=true
// adopts the proposed time into ScheduledFor and marks the sub-state
// 'confirmed'; confirmed=false marks it 'declined' and keeps the original
// reservation. Rows without an open 'requested' negotiation don't match —
// that (or a missing id) returns ErrRideRequestNotFound.
func (r *RideRequestRepo) ResolveReschedule(ctx context.Context, id string, confirmed bool) (RideRequestRecord, error) {
	start := time.Now()
	row := r.pool.QueryRow(ctx, queryRideRequestResolveReschedule, id, confirmed)
	rec, err := r.scanRideRequest(row)
	r.metrics.ObserveQueryDuration("ride_request.resolve_reschedule", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.ResolveReschedule(%s): %w", id, ErrRideRequestNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("ride_request.resolve_reschedule")
		return RideRequestRecord{}, fmt.Errorf("RideRequestRepo.ResolveReschedule(%s): %w", id, err)
	}
	return rec, nil
}
