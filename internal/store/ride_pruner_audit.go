// Audit-row construction for the ride retention sweep (MYR-447). Split out of
// ride_pruner.go so the grouping decision and its rationale sit in one place.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ridePrunerAuditMetadata is the P0-only metadata on a rides_pruned or
// ride_passengers_scrubbed row (CG-DL-5): a count and two timestamps of rows
// that have already been destroyed or emptied. No coordinates, no addresses, no
// place labels, no passenger names, no ride ids.
//
// The two dates are updated_at values, which is the same lower-bound-on-terminal
// instant the sweep's boundary is keyed off. They describe a window, not a row.
type ridePrunerAuditMetadata struct {
	RideCount      int    `json:"rideCount"`
	OldestRideDate string `json:"oldestRideDate"`
	NewestRideDate string `json:"newestRideDate"`
}

// insertRideAudits writes one audit row per (owner, vehicle) group in the batch,
// under the given action.
//
// GROUPING: BY VEHICLE, WITH userId = owner_id. This is the direct analogue of
// the drive pruner's §5.5 grouping, which is why it was chosen — the audit log
// already reads as "here is what the system did to this owner's car", and a ride
// pruned off a car belongs in that same story.
//
// IT IS A CHOICE, NOT AN INHERITANCE, AND IT HAS A COST WORTH NAMING: a ride has
// TWO parties. The row deleted here is as much the RIDER's record as the
// owner's — it carries their pickup and dropoff — and the rider gets NO
// rider-keyed audit row for its destruction. An owner subject-access request
// over "AuditLog" will surface the deletion; a rider's will not.
//
// We took that deliberately rather than emitting two rows per group. Doubling
// the audit volume to record the same batch twice would make the log harder to
// read for the case it is actually used for (reconstructing what the sweep did),
// and rider-side accountability is already served by the deletion being
// unconditional and contract-documented rather than discretionary — there is no
// per-rider decision here for an audit row to hold to account. If a rider-keyed
// view is ever required, the honest fix is a second row keyed by rider_id (which
// prunedRide would then have to carry), not a re-interpretation of this one.
//
// It reuses the same-package queryAuditInsert column list, so CG-DL-8 column
// parity with the Prisma model stays automatic (same reasoning as
// OwnerTeardown.insertAudit and insertPruneAudits).
func insertRideAudits(ctx context.Context, tx pgx.Tx, claimed []prunedRide, action AuditAction) (int, error) {
	order, groups := groupRidesByVehicle(claimed)

	now := time.Now().UTC()
	for _, vehicleID := range order {
		g := groups[vehicleID]
		meta, err := json.Marshal(ridePrunerAuditMetadata{
			RideCount:      g.count,
			OldestRideDate: g.oldest.UTC().Format(time.RFC3339),
			NewestRideDate: g.newest.UTC().Format(time.RFC3339),
		})
		if err != nil {
			return 0, fmt.Errorf("store.RidePruner.PruneBatch: marshal audit metadata: %w", err)
		}
		if _, err := tx.Exec(ctx, queryAuditInsert,
			newProvisionID(),           // id (cuid)
			g.ownerID,                  // userId (owner of the car the rides were booked against)
			now,                        // timestamp
			string(action),             // action
			auditTargetTypeRideRequest, // targetType
			vehicleID,                  // targetId (the owner's car)
			auditInitiatorSystemPruner, // initiator
			meta,                       // metadata (P0 counts + window)
			now,                        // createdAt
		); err != nil {
			return 0, fmt.Errorf("store.RidePruner.PruneBatch: insert audit: %w", err)
		}
	}
	return len(order), nil
}

// rideAuditGroup is one (owner, vehicle) group's tally.
type rideAuditGroup struct {
	ownerID string
	count   int
	oldest  time.Time
	newest  time.Time
}

// groupRidesByVehicle buckets claimed rows by vehicle, returning an
// insertion-ordered key list alongside the map so the audit rows land
// oldest-vehicle-first and the output is deterministic for tests.
func groupRidesByVehicle(claimed []prunedRide) (order []string, groups map[string]*rideAuditGroup) {
	order = make([]string, 0, len(claimed))
	groups = make(map[string]*rideAuditGroup, len(claimed))
	for i := range claimed {
		r := &claimed[i]
		g, ok := groups[r.vehicleID]
		if !ok {
			g = &rideAuditGroup{ownerID: r.ownerID, oldest: r.updatedAt, newest: r.updatedAt}
			groups[r.vehicleID] = g
			order = append(order, r.vehicleID)
		}
		g.count++
		if r.updatedAt.Before(g.oldest) {
			g.oldest = r.updatedAt
		}
		if r.updatedAt.After(g.newest) {
			g.newest = r.updatedAt
		}
	}
	return order, groups
}
