// RideRequestRepo drive-end completion accessor (MYR-265). The autonomous car
// parking at the dropoff (a drives.DriveEndedEvent for its VIN) ends the ride:
// the ride layer resolves VIN→vehicle cuid and calls CompleteEnrouteByVehicle
// to drive enroute→completed. Split from ride_request_repo.go so the
// lifecycle-transition surface there stays focused.

package store

import (
	"context"
	"fmt"
)

// CompleteEnrouteByVehicle transitions the vehicle's in-flight enroute ride to
// completed (leg-2 arrival at dropoff), stamping completed_at first-entry-only.
// The write is GUARDED on status = 'enroute' (queryRideRequestCompleteEnroute
// ByVehicle), so a drive-end for a vehicle with NO active ride matches zero
// rows and returns an empty slice — a clean no-op — and concurrent drive-ends
// serialize in the database (only the first completes the ride). Returns the
// completed record(s) so the caller can publish a ride_status_changed frame per
// row; in practice a vehicle serves at most one ride at a time, so this is 0 or
// 1 rows, but the slice tolerates the defensive multi-row case.
func (r *RideRequestRepo) CompleteEnrouteByVehicle(ctx context.Context, vehicleID string) ([]RideRequestRecord, error) {
	recs, err := r.list(ctx, "ride_request.complete_enroute_by_vehicle", queryRideRequestCompleteEnrouteByVehicle, vehicleID)
	if err != nil {
		return nil, fmt.Errorf("RideRequestRepo.CompleteEnrouteByVehicle(%s): %w", vehicleID, err)
	}
	return recs, nil
}
