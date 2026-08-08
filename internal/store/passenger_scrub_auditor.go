package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PassengerScrubAuditor writes the AuditLog rows for the ONE-TIME legacy
// passenger-column scrub (MYR-447), the `cmd/scrub-passenger-fields` binary.
//
// WHY THIS EXISTS SEPARATELY FROM THE SWEEPER'S EMITTER. The sweeper scrubs
// inside the same transaction as its claim and writes its audit rows on that
// transaction, which is what makes "audit before write" atomic there. The
// one-time binary has no such transaction: it is a long operator-run pass of
// per-row autocommit UPDATEs over the whole table, and wrapping a fleet-wide
// scrub in one transaction would hold locks for its entire duration. So the
// ordering guarantee it can offer is weaker and is stated plainly: every
// audit row is written, and confirmed, BEFORE the first column is cleared,
// and a failure to write one aborts the run before anything is scrubbed.
// What it does not offer is atomicity — a crash between the audit and the
// last UPDATE leaves rows recorded as scrubbed that are not yet. That
// direction is the safe one (the run is idempotent and simply re-runs), and
// it is the opposite of the direction CG-DL-3 cares about, which is a change
// that happened with no record of it.
//
// The action is the same `ride_passengers_scrubbed` the sweeper emits, on
// purpose: the promise is identical — a record survives with two fields
// removed — and splitting it by which tool did the work would make "when did
// this ride lose its passenger data?" two queries instead of one. The
// initiator distinguishes them.
type PassengerScrubAuditor struct {
	pool *pgxpool.Pool
}

// NewPassengerScrubAuditor builds the audit writer over the given pool.
func NewPassengerScrubAuditor(pool *pgxpool.Pool) *PassengerScrubAuditor {
	return &PassengerScrubAuditor{pool: pool}
}

// passengerScrubAuditMetadata is the P0-only metadata shape (CG-DL-5):
// a count and nothing else. Never a name, never a phone number — the scrub
// deliberately never reads the values it erases, so it could not log them
// even by accident.
type passengerScrubAuditMetadata struct {
	RideCount int    `json:"rideCount"`
	Source    string `json:"source"`
}

// RecordPassengerScrub writes one grouped audit row for the rides about to
// have both passenger columns cleared on one vehicle.
//
// initiator is `system_pruner` rather than `operator`: the row records a
// scheduled-policy action that happens to have been kicked off by hand
// rather than an operator READING somebody's data, and `metadata.source`
// carries the distinction for anyone reconstructing the timeline.
// `operator` is reserved for the access audit, where the question being
// answered is "who saw this?" — nobody saw anything here.
func (a *PassengerScrubAuditor) RecordPassengerScrub(
	ctx context.Context, ownerID, vehicleID string, rideCount int,
) error {
	if ownerID == "" || vehicleID == "" {
		return fmt.Errorf("store.RecordPassengerScrub: empty owner (%q) or vehicle (%q) id", ownerID, vehicleID)
	}
	meta, err := json.Marshal(passengerScrubAuditMetadata{
		RideCount: rideCount,
		Source:    "one_time_backfill",
	})
	if err != nil {
		return fmt.Errorf("store.RecordPassengerScrub: marshal audit metadata: %w", err)
	}
	now := time.Now().UTC()
	if _, err := a.pool.Exec(ctx, queryAuditInsert,
		newProvisionID(),
		ownerID,
		now,
		string(AuditActionRidePassengersScrubbed),
		auditTargetTypeRideRequest,
		vehicleID,
		auditInitiatorSystemPruner,
		meta,
		now,
	); err != nil {
		return fmt.Errorf("store.RecordPassengerScrub(vehicle=%s): %w", vehicleID, err)
	}
	return nil
}
