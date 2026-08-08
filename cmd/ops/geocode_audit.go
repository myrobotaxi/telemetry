// Operator-access audit for `ops geocode backfill` (MYR-447). Split out of
// geocode.go — which is already at the repo's 300-line file cap — so the
// grouping decision and its rationale sit in one place, mirroring how
// internal/store/ride_pruner_audit.go is split out of ride_pruner.go.

package main

import (
	"context"
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// geocodeBackfillAuditFields names what DriveRepo.ListMissingAddresses
// decrypts for EVERY row it returns: the GPS trail plus, since MYR-447,
// both sealed endpoint labels. Names only — store.OperatorAuditor rejects
// anything value-shaped (CG-DL-5).
//
// These are the DRIVE-side names, not the Vehicle ones in common.go's
// vehicleRowAuditFields: this command never reads a Vehicle row's location
// columns, it only joins "Vehicle" to learn who owns the car.
var geocodeBackfillAuditFields = []string{"routePoints", "startAddress", "endAddress"}

// operatorDecryptRecorder is the slice of *store.OperatorAuditor this file
// needs. Declared at the consumer per the repo's interfaces-at-consumer-site
// convention, and narrow enough that a fake can prove the fail-closed and
// grouping behaviour without a database.
type operatorDecryptRecorder interface {
	RecordDecrypt(ctx context.Context, access store.OperatorAccess) error
}

// auditGeocodeBackfill writes one operator_decrypt row per (owner, vehicle)
// group present in rows, and returns how many it wrote. Any insert failure
// aborts immediately: the caller must not geocode — must not transmit a
// single coordinate to Mapbox — on a non-nil error.
//
// GROUPING: BY VEHICLE, WITH userId = the vehicle's owner. Same shape as
// store.insertRideAudits and the drive pruner's §5.5 grouping, chosen for
// the same reason — the audit log reads as "here is what was done to this
// owner's car" — and here it is also a volume decision. A fleet-wide
// backfill can match tens of thousands of drives; a row per drive would
// flood an append-only table with near-identical entries and make the run
// harder to reconstruct, not easier. One row per car per run says exactly
// as much: this operator, at this time, opened the trails and endpoint
// labels of this owner's car.
//
// The cost, named rather than hidden: the row does not enumerate WHICH
// drives were opened. Reconstructing that means re-running the discovery
// predicate against the row's timestamp. That is an acceptable trade for a
// sweep whose scope is definitionally "everything eligible", and it would
// not be for a targeted lookup.
func auditGeocodeBackfill(
	ctx context.Context, auditor operatorDecryptRecorder, operator string, rows []store.DriveBackfillRow,
) (int, error) {
	order, owners := groupBackfillRowsByVehicle(rows)
	for _, vehicleID := range order {
		if err := auditor.RecordDecrypt(ctx, store.OperatorAccess{
			Operator:   operator,
			Command:    "ops geocode backfill",
			UserID:     owners[vehicleID],
			TargetType: store.OperatorTargetVehicle,
			// The Vehicle cuid, deliberately NOT the VIN: AuditLog columns
			// are P0 and a full VIN is restricted outside the owner's own
			// snapshot (data-classification.md §1.3, §2.1).
			TargetID: vehicleID,
			Fields:   geocodeBackfillAuditFields,
		}); err != nil {
			return 0, fmt.Errorf("vehicle %s: %w", vehicleID, err)
		}
	}
	return len(order), nil
}

// groupBackfillRowsByVehicle buckets rows by vehicle, returning an
// insertion-ordered key list alongside the vehicle→owner map so the audit
// rows land in the query's own oldest-drive-first order and the output is
// deterministic for tests.
func groupBackfillRowsByVehicle(rows []store.DriveBackfillRow) (order []string, owners map[string]string) {
	order = make([]string, 0, len(rows))
	owners = make(map[string]string, len(rows))
	for _, r := range rows {
		if _, seen := owners[r.VehicleID]; seen {
			continue
		}
		owners[r.VehicleID] = r.UserID
		order = append(order, r.VehicleID)
	}
	return order, owners
}
