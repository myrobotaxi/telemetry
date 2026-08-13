// The MULTI-STOP trip's stop list (MYR-539) — types, SQL, and the READ path.
//
// A stop is the middle of a trip: one pickup → N ordered stops → one
// destination, with the endpoints staying on the ride row. That shape is why
// the stops live in their own table rather than as more columns: the count is
// unbounded and the list is REPLACED wholesale by an edit, which is a set of
// rows, not a widening projection.
//
// WHY THE READ IS A SECOND QUERY. rideRequestColumns is already the widest
// projection in this repo (four AES-256-GCM decrypts and three identity
// subselects per row) and it is on every ride read there is — including the
// background probes that deliberately do NOT use it. Folding stops into it as a
// join would multiply every ride row by its stop count and make the lean reads'
// argument (ride_request_arrival.go, ride_request_active_poll.go) untrue by
// construction. So the surfaces that serve §7.8's RideRequest object attach
// stops with ONE extra statement — `ride_id = ANY($1)` for a whole page, not
// one query per row — and every other read stays exactly as lean as it was.
//
// Classification (data-classification.md §1.9): a stop's coordinates are P1 GPS
// data, encrypt-only in lat_enc/lng_enc and decrypted here; label/address are
// P1 log-redacted. Nothing in this file logs a place.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RideStopStatus mirrors the contracts RideStop.status enum. SERVER-OWNED: it
// advances on arrival detection (the MYR-538 waypoint events), never on a
// client tap, and an advance is a lifecycle transition — it does NOT bump
// trip_version, because the trip's SHAPE did not change.
type RideStopStatus string

const (
	// RideStopUpcoming — the vehicle has not started the leg ending here.
	RideStopUpcoming RideStopStatus = "upcoming"
	// RideStopCurrent — the leg the vehicle is driving NOW. At most one stop
	// per ride holds it; everything before it is completed.
	RideStopCurrent RideStopStatus = "current"
	// RideStopCompleted — the vehicle arrived here; the stop is behind the car.
	RideStopCompleted RideStopStatus = "completed"
)

// Frozen reports whether a stop is behind (or under) the car, and therefore
// cannot be removed, reordered or moved by a trip edit — the road behind the
// car cannot be rewritten.
func (s RideStopStatus) Frozen() bool {
	return s == RideStopCurrent || s == RideStopCompleted
}

// RideStop is one intermediate stop as the read path returns it: the
// server-minted id, the decrypted place, and the server-owned status. Position
// is implicit — the slice order IS the travel order, exactly as the wire array
// index is.
type RideStop struct {
	ID     string
	Place  RidePlace
	Status RideStopStatus
}

// RideStopEdit is one entry of the DESIRED list a client submits (contracts
// $defs.RideStopPatch). An empty ID means a brand-new stop the server mints an
// id for; a non-empty one names an existing stop to KEEP at this position.
type RideStopEdit struct {
	ID    string
	Place RidePlace
}

// rideStopIDRandomBytes sizes the random suffix of a stop id — the same 16
// bytes the ride id uses, under the "crs" prefix the contract's examples show
// (`crs0123456789abcdef0123456789abcd`).
const rideStopIDRandomBytes = rideRequestIDRandomBytes

// newRideStopID mints a stop id. SERVER-OWNED: a client never supplies one, so
// a stop it has just invented carries no id until this answers with one.
func newRideStopID() string {
	return "crs" + randomHex(rideStopIDRandomBytes)
}

// pgxQuerierRows is the multi-row read surface shared by *pgxpool.Pool and
// pgx.Tx — the stops read runs on the pool for an ordinary read and on the open
// transaction when a replacement write reads its own result back. Consumer-site
// per convention, alongside pgxQuerier's single-row twin.
type pgxQuerierRows interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// pgxRowQuerier is both halves at once — the surface a write path needs when it
// runs one guarded statement and then reads the stop list back through whatever
// it is running on (the pool, or an open transaction).
type pgxRowQuerier interface {
	pgxQuerier
	pgxQuerierRows
}

// queryRideStopsByRides loads the stops of one OR MANY rides in travel order.
// `ride_id = ANY($1)` is what keeps the list surfaces at two statements per
// page instead of two per row; idx_go_ride_stops_ride_position serves both the
// filter and the ordering.
const queryRideStopsByRides = `SELECT ride_id, id, lat_enc, lng_enc, label, address, status
FROM go_ride_stops
WHERE ride_id = ANY($1)
ORDER BY ride_id, position`

// loadStops returns the stops of every requested ride, keyed by ride id and in
// travel order. Rides with no stops are simply absent from the map — the
// contract reads absence as "no stops", never as "unknown".
func (r *RideRequestRepo) loadStops(ctx context.Context, q pgxQuerierRows, rideIDs []string) (map[string][]RideStop, error) {
	out := make(map[string][]RideStop)
	if len(rideIDs) == 0 {
		return out, nil
	}
	const op = "ride_request.load_stops"
	start := time.Now()
	rows, err := q.Query(ctx, queryRideStopsByRides, rideIDs)
	if err != nil {
		r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("load ride stops: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			rideID string
			stop   RideStop
			latEnc string
			lngEnc string
			status string
		)
		if err := rows.Scan(&rideID, &stop.ID, &latEnc, &lngEnc, &stop.Place.Label, &stop.Place.Address, &status); err != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("load ride stops: scan: %w", err)
		}
		stop.Status = RideStopStatus(status)
		if stop.Place.Latitude, err = r.decryptCoord("stop lat_enc", latEnc); err != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("load ride stops: %w", err)
		}
		if stop.Place.Longitude, err = r.decryptCoord("stop lng_enc", lngEnc); err != nil {
			r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
			r.metrics.IncQueryError(op)
			return nil, fmt.Errorf("load ride stops: %w", err)
		}
		out[rideID] = append(out[rideID], stop)
	}
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("load ride stops: rows: %w", err)
	}
	return out, nil
}

// attachStops fills in the Stops field of every record in place — ONE extra
// statement for the whole slice. A ride with no stops keeps a nil slice, which
// is the wire's omitted key.
//
// It is deliberately called at the FUNNELS that serve the wire RideRequest
// object (the detail read, the paged lists, the guarded writes' RETURNING row)
// rather than inside scanRideRequest: the lean background reads share the
// scanner's crypto helpers but must never acquire its cost.
func (r *RideRequestRepo) attachStops(ctx context.Context, q pgxQuerierRows, recs []RideRequestRecord) error {
	if len(recs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(recs))
	for i := range recs {
		ids = append(ids, recs[i].ID)
	}
	byRide, err := r.loadStops(ctx, q, ids)
	if err != nil {
		return err
	}
	if len(byRide) == 0 {
		return nil
	}
	for i := range recs {
		if stops, ok := byRide[recs[i].ID]; ok {
			recs[i].Stops = stops
		}
	}
	return nil
}

// withStops is attachStops for a single record, returned by value so the
// single-row read paths stay one-liners.
func (r *RideRequestRepo) withStops(ctx context.Context, q pgxQuerierRows, rec RideRequestRecord) (RideRequestRecord, error) {
	recs := []RideRequestRecord{rec}
	if err := r.attachStops(ctx, q, recs); err != nil {
		return RideRequestRecord{}, err
	}
	return recs[0], nil
}
