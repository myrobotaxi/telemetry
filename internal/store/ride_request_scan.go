// RideRequestRepo scan + coordinate-crypto helpers (MYR-173). Split from
// ride_request_repo.go so the accessor file stays focused on the public
// surface. Reuses the lossless float<->string codec from
// vehicle_gps_encryption.go (strconv round-trip, byte-compatible with the
// TS helpers) so a future cross-repo reader decrypts identically.

package store

import (
	"fmt"

	"github.com/jackc/pgx/v5"
)

// encryptPlace encrypts a place's coordinate pair for the *_enc columns.
// Unlike the Vehicle dual-write path there is no plaintext fallback column,
// so any encrypt failure fails the write.
func (r *RideRequestRepo) encryptPlace(p RidePlace) (latEnc, lngEnc string, err error) {
	lat := p.Latitude
	lng := p.Longitude
	latEnc, err = floatToEncString(&lat, r.encryptor)
	if err != nil {
		return "", "", fmt.Errorf("encrypt latitude: %w", err)
	}
	lngEnc, err = floatToEncString(&lng, r.encryptor)
	if err != nil {
		return "", "", fmt.Errorf("encrypt longitude: %w", err)
	}
	return latEnc, lngEnc, nil
}

// decryptCoord decrypts one required coordinate column. A NULL/empty or
// non-numeric result is corruption (the column is NOT NULL and encrypt-only
// — there is no legitimate absent state), so it escalates to an error
// rather than the (nil, nil) absent sentinel the Vehicle fallback path
// tolerates.
func (r *RideRequestRepo) decryptCoord(column, ciphertext string) (float64, error) {
	v, err := encStringToFloat(&ciphertext, r.encryptor)
	if err != nil {
		return 0, fmt.Errorf("decrypt %s: %w", column, err)
	}
	if v == nil {
		return 0, fmt.Errorf("decrypt %s: corrupt or empty ciphertext", column)
	}
	return *v, nil
}

// scanRideRequest scans one rideRequestColumns row and decrypts the
// coordinate ciphertexts into the returned record. pgx.ErrNoRows passes
// through unwrapped so callers can map it to ErrRideRequestNotFound.
func (r *RideRequestRepo) scanRideRequest(row pgx.Row) (RideRequestRecord, error) {
	var (
		rec             RideRequestRecord
		pickupLatEnc    string
		pickupLngEnc    string
		dropoffLatEnc   string
		dropoffLngEnc   string
		status          string
		reschedStatus   *string
		dispatchStatus  *string
		requesterName   *string
		requesterEmail  *string
		requesterExists bool
		joinCode        *string
	)
	err := row.Scan(
		&rec.ID, &rec.RiderID, &rec.OwnerID, &rec.VehicleID,
		&pickupLatEnc, &pickupLngEnc, &rec.Pickup.Label, &rec.Pickup.Address,
		&dropoffLatEnc, &dropoffLngEnc, &rec.Dropoff.Label, &rec.Dropoff.Address,
		&status, &rec.PassengerName, &rec.PassengerPhone,
		&rec.ScheduledFor, &rec.RescheduleProposedFor, &reschedStatus,
		&rec.AcceptedAt, &rec.CompletedAt, &rec.CreatedAt, &rec.UpdatedAt,
		&dispatchStatus, &rec.DispatchedAt, &rec.DispatchError,
		&rec.CancelledBy, &rec.TripVersion,
		&rec.GroupRide, &joinCode, &rec.JoinCodeExpiresAt,
		&requesterName, &requesterEmail, &requesterExists,
	)
	if err != nil {
		// %w keeps pgx.ErrNoRows visible to the callers' errors.Is mapping.
		return RideRequestRecord{}, fmt.Errorf("scan ride request: %w", err)
	}

	rec.Status = RideRequestStatus(status)
	// Requester display name (MYR-229): resolved from the same statement's
	// inline "User" subselect. A missing rider row leaves RequesterName "" so
	// the wire projections OMIT the field — a NULL identity NEVER fails a read.
	if requesterExists {
		rec.RequesterName = requesterDisplayName(requesterName, requesterEmail)
	}
	if reschedStatus != nil {
		rs := RescheduleStatus(*reschedStatus)
		rec.RescheduleStatus = &rs
	}
	if dispatchStatus != nil {
		ds := DispatchStatus(*dispatchStatus)
		rec.DispatchStatus = &ds
	}
	// The join code is NULL on every solo ride and on a group ride the owner has
	// not accepted yet (MYR-540). It lands as the empty string, which is the
	// "no live code" sentinel every reader here already tests for — and which
	// keeps a P1 bearer credential out of a pointer that could be logged with
	// %+v.
	if joinCode != nil {
		rec.JoinCode = *joinCode
	}

	if rec.Pickup.Latitude, err = r.decryptCoord("pickup_lat_enc", pickupLatEnc); err != nil {
		return RideRequestRecord{}, err
	}
	if rec.Pickup.Longitude, err = r.decryptCoord("pickup_lng_enc", pickupLngEnc); err != nil {
		return RideRequestRecord{}, err
	}
	if rec.Dropoff.Latitude, err = r.decryptCoord("dropoff_lat_enc", dropoffLatEnc); err != nil {
		return RideRequestRecord{}, err
	}
	if rec.Dropoff.Longitude, err = r.decryptCoord("dropoff_lng_enc", dropoffLngEnc); err != nil {
		return RideRequestRecord{}, err
	}
	return rec, nil
}
