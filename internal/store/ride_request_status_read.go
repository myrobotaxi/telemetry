// A deliberately LEAN ride-status read (MYR-527).
//
// The nav-apply verifier polls "is this leg still the ride's current leg?"
// from a background goroutine; the wide record read drags the requester
// identity subselects and four coordinate decrypts it has no use for. One
// column, following the VehicleNameRepo / VehiclePosition precedent.

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// queryRideRequestStatusOnly reads a ride's bare lifecycle status.
const queryRideRequestStatusOnly = `SELECT status FROM go_ride_requests WHERE id = $1`

// RideStatus returns the ride's current lifecycle status. A ride that does
// not exist yields ErrRideRequestNotFound.
func (r *RideRequestRepo) RideStatus(ctx context.Context, rideID string) (string, error) {
	var status string
	if err := r.pool.QueryRow(ctx, queryRideRequestStatusOnly, rideID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("store.RideStatus(%s): %w", rideID, ErrRideRequestNotFound)
		}
		return "", fmt.Errorf("store.RideStatus(%s): %w", rideID, err)
	}
	return status, nil
}
