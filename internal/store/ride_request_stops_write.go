// The REPLACEMENT write for a ride's stop list (MYR-539), folded into the
// MYR-541 guarded trip write: same transaction, same trip_version guard, same
// status window. There is ONE version for the whole trip shape, so a stop edit
// and an endpoint edit are the same kind of event and must be arbitrated by the
// same statement — not by two writes that could interleave.
//
// REPLACE, NOT DIFF OPS (the contract's decision, and the reason this file has
// no "insert at index" verb): the client sends the full desired list, an entry
// with an id keeps that stop, an entry without one is new, and an existing stop
// left out is removed. tripVersion already serializes the editors, so a list is
// unambiguous where an operation replayed against a base its sender never saw
// is not.
//
// WHAT IS REFUSED. The road behind the car cannot be rewritten: a replacement
// that REMOVES or REORDERS a stop the car has already reached ('completed') or
// is driving to right now ('current') is refused with the conflict sentinel —
// the whole edit, not a filtered version of it, because the client sends what
// it holds and lets the guard decide. A frozen stop's PLACE is likewise
// unwritable, and that one is silent rather than a refusal: the client restates
// every entry's place by construction (RideStopPatch requires it), so treating
// a byte-difference on a stop nobody is editing as a 409 would refuse edits to
// the rest of the trip for no reason a user could act on.

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrRideStopUnknown is returned when a submitted entry names a stop id the
// ride does not have. It is a BAD REQUEST, not a conflict: the contract is
// explicit that an unknown id is never silently treated as a new stop, and it
// cannot be resolved by refetching-and-retrying the way a stale version can.
// The HTTP layer maps it to 400 invalid_request.
var ErrRideStopUnknown = errors.New("ride stop not on this ride")

const (
	// queryRideStopsForUpdate is the replacement's own snapshot of the current
	// list, taken inside the transaction that already holds the ride row. FOR
	// UPDATE locks the stop rows so a concurrent leg-advance (which freezes a
	// stop) cannot land between this read and the writes planned from it.
	queryRideStopsForUpdate = `SELECT id, status
FROM go_ride_stops
WHERE ride_id = $1
ORDER BY position
FOR UPDATE`

	queryRideStopDelete = `DELETE FROM go_ride_stops WHERE ride_id = $1 AND id = $2`

	// queryRideStopInsert mints a new stop. status defaults to 'upcoming' —
	// a stop is born ahead of the car.
	queryRideStopInsert = `INSERT INTO go_ride_stops (
	id, ride_id, position, lat_enc, lng_enc, label, address
) VALUES ($1, $2, $3, $4, $5, $6, $7)`

	// queryRideStopMove re-numbers and re-places an EXISTING stop. The status
	// is untouched (server-owned, lifecycle-only) and so is the place of a
	// FROZEN stop — $8 carries that decision as a bind rather than as a second
	// statement, so the whole list is written by one prepared query.
	queryRideStopMove = `UPDATE go_ride_stops SET
	position   = $3,
	lat_enc    = CASE WHEN $8 THEN lat_enc ELSE $4 END,
	lng_enc    = CASE WHEN $8 THEN lng_enc ELSE $5 END,
	label      = CASE WHEN $8 THEN label   ELSE $6 END,
	address    = CASE WHEN $8 THEN address ELSE $7 END,
	updated_at = NOW()
WHERE ride_id = $1 AND id = $2`

	// queryRideStopPromoteFirst makes the earliest UPCOMING stop the CURRENT
	// one, and only when the ride has no current stop already. It is the
	// invariant "a ride under way with stops ahead of it always has exactly one
	// current stop", expressed as a single idempotent statement — which is what
	// makes it safe on both of its callers (the rider's Start, and an edit that
	// adds the first stop to a ride already en route).
	queryRideStopPromoteFirst = `UPDATE go_ride_stops SET
	status = 'current',
	updated_at = NOW()
WHERE id = (
	SELECT id FROM go_ride_stops
	WHERE ride_id = $1 AND status = 'upcoming'
	  AND NOT EXISTS (SELECT 1 FROM go_ride_stops WHERE ride_id = $1 AND status = 'current')
	ORDER BY position
	LIMIT 1
)
RETURNING id, lat_enc, lng_enc, label, address`
)

// stopWrite is one planned row write: an existing stop moved to Position, or a
// new stop (empty ID) to mint there.
type stopWrite struct {
	ID       string
	Position int
	Place    RidePlace
	// Frozen marks a current/completed stop: its position was ASSERTED
	// unchanged by the planner, and its place must not be overwritten.
	Frozen bool
}

// stopPlan is a whole replacement resolved against the current list.
type stopPlan struct {
	Writes  []stopWrite
	Deletes []string
}

// planStopReplacement turns (current list, desired list) into the row writes
// that realize it, or the refusal that stops it. PURE — no database, no ids
// minted, no clock — because every rule worth arguing about lives here and a
// rule that needs a container to test is a rule nobody re-reads.
//
// existing MUST be in travel order (position order); its index is each stop's
// current position.
func planStopReplacement(existing []RideStop, desired []RideStopEdit) (stopPlan, error) {
	byID := make(map[string]int, len(existing))
	for i := range existing {
		byID[existing[i].ID] = i
	}

	plan := stopPlan{Writes: make([]stopWrite, 0, len(desired))}
	kept := make(map[string]struct{}, len(desired))
	for i := range desired {
		d := desired[i]
		if d.ID == "" {
			plan.Writes = append(plan.Writes, stopWrite{Position: i, Place: d.Place})
			continue
		}
		pos, ok := byID[d.ID]
		if !ok {
			return stopPlan{}, fmt.Errorf("stop %s: %w", d.ID, ErrRideStopUnknown)
		}
		if _, dup := kept[d.ID]; dup {
			return stopPlan{}, fmt.Errorf("stop %s listed twice: %w", d.ID, ErrRideStopUnknown)
		}
		kept[d.ID] = struct{}{}

		frozen := existing[pos].Status.Frozen()
		// A frozen stop may not MOVE. Requiring its exact index (rather than
		// only its relative order) is deliberate: inserting a new stop ahead of
		// a completed one would renumber the road already travelled, which is
		// the same lie as reordering it.
		if frozen && pos != i {
			return stopPlan{}, fmt.Errorf(
				"stop %s is %s and cannot be reordered: %w", d.ID, existing[pos].Status, ErrRideRequestConflict)
		}
		plan.Writes = append(plan.Writes, stopWrite{ID: d.ID, Position: i, Place: d.Place, Frozen: frozen})
	}

	for i := range existing {
		if _, ok := kept[existing[i].ID]; ok {
			continue
		}
		if existing[i].Status.Frozen() {
			return stopPlan{}, fmt.Errorf(
				"stop %s is %s and cannot be removed: %w", existing[i].ID, existing[i].Status, ErrRideRequestConflict)
		}
		plan.Deletes = append(plan.Deletes, existing[i].ID)
	}
	return plan, nil
}

// replaceStops applies a desired list to a ride inside an OPEN transaction that
// already won the guarded trip UPDATE (so the status window and the version
// match are settled before a single stop row is touched). Returns the resulting
// list, in travel order, read back from the same transaction.
func (r *RideRequestRepo) replaceStops(
	ctx context.Context, tx pgx.Tx, rideID string, status RideRequestStatus, desired []RideStopEdit,
) ([]RideStop, error) {
	existing, err := readStopsForUpdate(ctx, tx, rideID)
	if err != nil {
		return nil, err
	}
	plan, err := planStopReplacement(existing, desired)
	if err != nil {
		return nil, err
	}

	// Deletes first: the list is re-numbered from scratch below, and removing
	// the departing rows before renumbering keeps every intermediate state of
	// the transaction free of duplicate positions.
	for _, id := range plan.Deletes {
		if _, err := tx.Exec(ctx, queryRideStopDelete, rideID, id); err != nil {
			return nil, fmt.Errorf("delete ride stop %s: %w", id, err)
		}
	}
	for i := range plan.Writes {
		if err := r.writeStop(ctx, tx, rideID, plan.Writes[i]); err != nil {
			return nil, err
		}
	}

	// An edit that gives a ride ALREADY UNDER WAY its first stop has just moved
	// the current leg's target; promoting the earliest upcoming stop is what
	// makes the arrival detector watch it and the re-share aim at it.
	//
	// THE ONLY GAP THIS CAN BE CLOSING IS "no current stop" (MYR-550). The
	// symmetric case — a replacement that leaves the previous current stop
	// ABSENT — is unreachable, because planStopReplacement above has already
	// refused it: an omitted stop whose status is Frozen() (current or
	// completed) is ErrRideRequestConflict, so control never arrives here with
	// a current stop deleted. That is why one idempotent promote is the whole
	// fix and there is no "demote/re-promote" arm to go with it.
	//
	// The window is enroute ONLY, on purpose. A requested/accepted/arrived ride
	// has not started, its stops are all legitimately upcoming, and promoting
	// one early would point the MYR-538 detector at a stop while the car is
	// still driving to the PICKUP. Promotion for that phase belongs to
	// StartFirstStop and stays there.
	if status == RideRequestStatusEnroute {
		if _, err := r.promoteFirstUpcomingStop(ctx, tx, rideID); err != nil {
			return nil, err
		}
	}

	byRide, err := r.loadStops(ctx, tx, []string{rideID})
	if err != nil {
		return nil, err
	}
	return byRide[rideID], nil
}

// writeStop realizes one planned write: an UPDATE for a kept stop, an INSERT
// (with a freshly minted id) for a new one.
func (r *RideRequestRepo) writeStop(ctx context.Context, tx pgx.Tx, rideID string, w stopWrite) error {
	latEnc, lngEnc, err := r.encryptPlace(w.Place)
	if err != nil {
		return fmt.Errorf("encrypt ride stop place: %w", err)
	}
	if w.ID == "" {
		if _, err := tx.Exec(ctx, queryRideStopInsert,
			newRideStopID(), rideID, w.Position, latEnc, lngEnc, w.Place.Label, w.Place.Address,
		); err != nil {
			return fmt.Errorf("insert ride stop: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, queryRideStopMove,
		rideID, w.ID, w.Position, latEnc, lngEnc, w.Place.Label, w.Place.Address, w.Frozen,
	); err != nil {
		return fmt.Errorf("move ride stop %s: %w", w.ID, err)
	}
	return nil
}

// promoteFirstUpcomingStop makes the earliest upcoming stop current when the
// ride has none, and returns it. A ride with no upcoming stops (or one that
// already has a current stop) matches no row and returns a nil stop — not an
// error: both are ordinary states, and the statement is written to be safe to
// run against either.
func (r *RideRequestRepo) promoteFirstUpcomingStop(ctx context.Context, q pgxQuerier, rideID string) (*RideStop, error) {
	var (
		stop   RideStop
		latEnc string
		lngEnc string
	)
	err := q.QueryRow(ctx, queryRideStopPromoteFirst, rideID).Scan(
		&stop.ID, &latEnc, &lngEnc, &stop.Place.Label, &stop.Place.Address)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // "no stop to promote" is an ordinary, non-error state
	}
	if err != nil {
		return nil, fmt.Errorf("promote first upcoming ride stop: %w", err)
	}
	if stop.Place.Latitude, err = r.decryptCoord("stop lat_enc", latEnc); err != nil {
		return nil, fmt.Errorf("promote first upcoming ride stop: %w", err)
	}
	if stop.Place.Longitude, err = r.decryptCoord("stop lng_enc", lngEnc); err != nil {
		return nil, fmt.Errorf("promote first upcoming ride stop: %w", err)
	}
	stop.Status = RideStopCurrent
	return &stop, nil
}

// readStopsForUpdate is the locked, ordered snapshot the planner works from.
// It reads NO coordinates: the plan only needs each stop's identity, order and
// status, and decrypting places the write is about to overwrite would be work
// done for nothing.
func readStopsForUpdate(ctx context.Context, tx pgx.Tx, rideID string) ([]RideStop, error) {
	rows, err := tx.Query(ctx, queryRideStopsForUpdate, rideID)
	if err != nil {
		return nil, fmt.Errorf("read ride stops for update: %w", err)
	}
	defer rows.Close()

	out := make([]RideStop, 0)
	for rows.Next() {
		var (
			stop   RideStop
			status string
		)
		if err := rows.Scan(&stop.ID, &status); err != nil {
			return nil, fmt.Errorf("read ride stops for update: scan: %w", err)
		}
		stop.Status = RideStopStatus(status)
		out = append(out, stop)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ride stops for update: rows: %w", err)
	}
	return out, nil
}
