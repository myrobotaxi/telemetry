// A deliberately LEAN vehicle-position read (MYR-535).
//
// The reservation sweeper computes its dispatch lead from the car's current
// position — "leave early enough to ARRIVE at scheduledFor" — and it asks on
// every tick for every reservation inside the lead window. VehicleRepo.GetByID
// is the ~70-column snapshot read with a control-state join; this is two
// ciphertext columns and one decrypt, following the VehicleNameRepo precedent
// (wide selects belong to detail/edit handlers).
//
// The pair decrypts through the SAME resolveGPSPair every other read uses, so
// the sweeper, the catalog and the snapshot can never disagree about where a
// car is. A nil pair means the server holds no fix for the car — the sweeper
// falls back to its floor lead rather than estimating from nothing.
//
// The "Vehicle" table is Prisma-owned and READ-ONLY here (CG-DL-9).

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// queryVehiclePosition reads the main GPS ciphertext pair by vehicle cuid.
const queryVehiclePosition = `SELECT "latitudeEnc", "longitudeEnc" FROM "Vehicle" WHERE "id" = $1`

// VehiclePosition returns the car's freshest known position, decrypted, or a
// nil pair when the server holds none (never streamed, half-pair corruption,
// or a repo built without an encryptor). A vehicle that does not exist yields
// ErrVehicleNotFound. The pair is ATOMIC: both non-nil or both nil, enforced
// by resolveGPSPair.
//
// Note the (0, 0) NO-FIX SENTINEL (vehicle-state-schema.md §2.3) can decrypt
// to a non-nil pair here — storage does not collapse it. Callers computing
// anything from the position must treat (0, 0) as "no fix", exactly as the
// wire layer collapses it to null.
func (r *VehicleRepo) VehiclePosition(ctx context.Context, vehicleID string) (lat, lng *float64, err error) {
	var gps summaryGPSScan
	if err := r.pool.QueryRow(ctx, queryVehiclePosition, vehicleID).Scan(gps.latDest(), gps.lngDest()); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("store.VehiclePosition(%s): %w", vehicleID, ErrVehicleNotFound)
		}
		return nil, nil, fmt.Errorf("store.VehiclePosition(%s): %w", vehicleID, err)
	}
	lat, lng = gps.resolve(r)
	return lat, lng, nil
}
