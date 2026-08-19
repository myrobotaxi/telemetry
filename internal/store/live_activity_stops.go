// The Live Activity's view of a multi-stop trip (MYR-587).
//
// Both content-state read paths — the lifecycle fan-out's single row and the
// ticker's per-pass list — need the same extra fact: which place is the car
// actually driving to right now. On a two-endpoint ride that is the drop-off
// the ride row already carries, and this file returns nothing at all. On a
// multi-stop trip it is a row in go_ride_stops, and the card was naming the
// drop-off beside an ETA that belonged to the stop (MYR-485: the wire ETA is
// always about the leg the car is CURRENTLY driving).
//
// WHY THIS IS ITS OWN STATEMENT rather than more columns on the two content-
// state queries. A stop list is rows, not a widening projection, so folding it
// in as a join would multiply every Activity row by its ride's stop count and
// make the ticker's "one statement per pass" untrue. Keyed `ride_id = ANY($1)`
// it stays ONE statement for a whole pass — the same shape ride_request_stops.go
// uses for a whole page — and the lifecycle path passes a single id through it.
//
// WHY NOT loadStops, which already reads this table: it decrypts lat_enc and
// lng_enc on every row. The card names a place, it does not plot one, so this
// projection takes the two columns the copy rule reads and performs no
// AES-256-GCM work at all — on a read that runs for every live Activity every
// 24–36 seconds.
//
// Classification (data-classification.md §1.9, §1.18): `label` is P1
// log-redacted, exactly like the `dropoff_label` it stands in for on the wire.
// Nothing in this file logs a place.

package store

import (
	"context"
	"fmt"
)

// ActivityStop is one intermediate stop as the content-state projection needs
// it: the server-owned status that decides whether the car has passed it, and
// the label the card would print. Position is implicit — the slice order IS the
// travel order, as it is everywhere else this table is read.
type ActivityStop struct {
	Status RideStopStatus
	Label  string
}

// queryActivityStopsByRides reads the stop lists of one OR MANY rides in travel
// order. Served by idx_go_ride_stops_ride_position for both the filter and the
// ordering, like every other read of this table.
const queryActivityStopsByRides = `SELECT ride_id, status, label
FROM go_ride_stops
WHERE ride_id = ANY($1)
ORDER BY ride_id, position`

// ActivityStopsForRides returns each requested ride's stops in travel order,
// keyed by ride id.
//
// A ride with no stops is simply ABSENT from the map, which is the ordinary
// case: most trips are pickup → drop-off, and the caller reads a missing key as
// "the drop-off is the only place left", exactly as it did before this existed.
func (r *LiveActivityRepo) ActivityStopsForRides(ctx context.Context, rideIDs []string) (map[string][]ActivityStop, error) {
	out := make(map[string][]ActivityStop)
	if len(rideIDs) == 0 {
		return out, nil
	}

	rows, err := r.pool.Query(ctx, queryActivityStopsByRides, rideIDs)
	if err != nil {
		return nil, fmt.Errorf("store.ActivityStopsForRides: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rideID string
		var stop ActivityStop
		if err := rows.Scan(&rideID, &stop.Status, &stop.Label); err != nil {
			return nil, fmt.Errorf("store.ActivityStopsForRides: scan: %w", err)
		}
		out[rideID] = append(out[rideID], stop)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ActivityStopsForRides: iterate: %w", err)
	}
	return out, nil
}

// attachStops fills the multi-stop half of every listed leg in ONE statement
// for the whole pass (MYR-587).
//
// Two Activities on one ride — a group ride's members each hold their own
// registration — are one key in the map and one read, and they must be given
// the same list: the stops belong to the trip, not to whoever is watching it.
// A pass with no listed Activity, which is the ordinary state, issues no
// statement at all.
//
// NOT FILTERED BY RIDE STATUS, deliberately. The copy rule reads the list only
// on `enroute` today (push.legDestination), and narrowing the read to that
// status would encode one package's copy decision in the other's SQL — so the
// day the card wants to say "At {stop}" while the car dwells, the projection
// would silently deliver nothing and the bug would look like a copy bug. The
// read is one index scan over at most a pass's worth of ride ids, which is not
// a budget worth buying that coupling with.
func (r *LiveActivityRepo) attachStops(ctx context.Context, legs []LiveActivityLeg) error {
	if len(legs) == 0 {
		return nil
	}

	rideIDs := make([]string, 0, len(legs))
	seen := make(map[string]struct{}, len(legs))
	for i := range legs {
		id := legs[i].RideRequestID
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		rideIDs = append(rideIDs, id)
	}

	stops, err := r.ActivityStopsForRides(ctx, rideIDs)
	if err != nil {
		return fmt.Errorf("store.ListActiveLegActivities: %w", err)
	}
	for i := range legs {
		legs[i].Stops = stops[legs[i].RideRequestID]
	}
	return nil
}
