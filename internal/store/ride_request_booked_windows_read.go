// The picker READ surface for the MYR-383 booking gate (MYR-385): "which
// intervals would this vehicle refuse a reservation in?"
//
// WHY IT EXISTS. The gate refuses a colliding reservation at CREATE with
// `409 vehicle_unavailable` / `time_conflict`. In r15 that refusal was the
// FIRST thing a rider heard — the schedule picker happily offered noon for a car
// the rider had themselves already booked at noon — and a refusal that arrives
// only after the rider has committed to a choice reads as a bug rather than as
// a rule. This read lets the picker dim the slot instead.
//
// THE INVARIANT, and it is the whole design: an interval this returns is
// exactly an interval the gate would refuse, and an instant outside every
// returned interval is exactly one the gate would allow. That is not maintained
// by care; it is maintained by CONSTRUCTION. The query below is assembled from
// the very fragments rideWindowConflictPredicate is assembled from —
// rideWindowReservationOccupies / rideWindowInstantOccupies for "which rides
// count", rideWindowOverlap for "how wide" — so there is no second spelling of
// the rule to drift, and RideConflictWindow reaches both through the same bind
// parameter. Widening or narrowing that const moves the gate and every client's
// picker together, on the next response, with no client release.
//
// WHAT THE CALLER LEARNS, and nothing more. Two instants and two booleans per
// occupying ride. The conflicting ride's id, rider, requester name, pickup and
// dropoff never leave this statement — they are P1 and belong to a party the
// caller is not (data-classification.md §1.9). The instants and the pending
// flag are P0 operational timing, the same tier as `status` and the same
// disclosure the gate's own refusal message already makes when it names the
// conflicting instant. This surface therefore grants NO capability its caller
// lacked: somebody authorised to book this car can already discover the same
// instants one create attempt at a time, because each 409 names the nearest
// one. It only makes the answer cheap enough to render a picker with.
//
// FILENAME, and it is not arbitrary: the `_read` suffix exists solely to keep
// this file from ending in `_windows.go`, which the Go toolchain reads as the
// GOOS build constraint `windows` and SILENTLY excludes from a darwin/linux
// build — no error, just a package missing a method. The same trap applies to
// the test file. Do not rename either back.

package store

import (
	"context"
	"fmt"
	"time"
)

// BookedWindow is one interval in which a vehicle cannot take a new
// reservation, as the picker needs to render it.
//
// Start and End are CONCRETE instants, already resolved against
// RideConflictWindow — deliberately not an anchor the client re-centres. The
// half-width is a product guess (see RideConflictWindow) that appears in one
// place on this server and in no schema, enum or client, and emitting resolved
// endpoints is what keeps it that way.
//
// The interval is OPEN at both ends, inherited from the gate's strict
// comparison: a reservation for exactly Start or exactly End is ALLOWED. A
// consumer dims `Start < slot < End`.
type BookedWindow struct {
	// Start is the instant the blocked interval begins, exclusive.
	Start time.Time
	// End is the instant it ends, exclusive. End.Sub(Start) is always exactly
	// 2*RideConflictWindow, identical for every window in one response.
	End time.Time
	// Pending reports that the claim is a still-undecided `requested`
	// reservation rather than a committed one, so a caller can say "already
	// requested" instead of the untrue "already booked". It changes the WORDS
	// only: a pending claim blocks a create exactly as hard as a committed one,
	// because the create path counts pending claims in full (see
	// bookedWindowsCountPending).
	Pending bool
	// Own reports that the occupying ride is one the CALLER requested — they
	// are its rider. Purely presentational; the gate does not care whose the
	// other ride is. False says only "not yours", never who.
	Own bool
}

// MaxBookedWindowRange caps the [from, to) span one call may ask about. The
// HTTP layer refuses a wider range rather than truncating it, because a
// silently shortened answer is a picker that under-dims — the one failure mode
// this surface exists to remove.
//
// 14 days is the horizon a schedule picker plausibly shows, and it is also what
// BOUNDS the response: the gate refuses new bookings closer together than
// RideConflictWindow, so open rides on one car are normally at least that far
// apart, which puts a ceiling of a few hundred windows on any legal range. That
// bound is why the query carries no LIMIT — a LIMIT would trade an impossible
// runaway for a possible silent truncation, and truncation here breaks the
// invariant this whole file is built to hold.
const MaxBookedWindowRange = 14 * 24 * time.Hour

// bookedWindowsCountPending is the count-pending flag bound at $5, and it is
// TRUE because this surface answers for the CREATE path.
//
// The gate's two landing sites disagree about a merely `requested` reservation
// on purpose (see rideWindowReservationOccupies): create counts it, accept does
// not. A picker is a rider deciding what to create, so it must see exactly what
// create would see. Showing the accept-side answer would hand a rider a slot
// that create is going to refuse — reintroducing the late 409 this endpoint
// exists to prevent, and doing it in the least obvious way possible.
const bookedWindowsCountPending = true

// queryVehicleBookedWindows selects the windows on one vehicle that OVERLAP the
// caller's range. $1 vehicle_id, $2 range start, $3 range end, $4 the window
// half-width in seconds, $5 whether pending claims count, $6 the caller.
//
// $1, $4 and $5 sit at the same positions queryRideWindowConflict binds them
// at, which is the positional contract the shared fragments carry. $2/$3 are a
// RANGE here rather than a single proposed instant, and that inversion is the
// entire difference between the two statements.
//
// OVERLAP IS STRICT on both sides — `start < to AND end > from` — for the same
// reason the gate's comparison is: the windows are OPEN intervals, so one that
// merely TOUCHES the requested range at a boundary blocks nothing inside it and
// would be noise in the response.
//
// The WHERE clause spells that overlap through rideWindowOverlap, which moves
// the half-width onto the BOUNDS so `r.scheduled_for` stands bare — `anchor >
// $2 - W AND anchor < $3 + W` rather than the equivalent `start < $3 AND end >
// $2`. The COALESCE'd rideWindowStartExpr / rideWindowEndExpr survive in the
// SELECT list, where the picker actually needs two concrete instants and where
// an expression costs nothing. Read rideWindowOverlap before touching either;
// swapping the projection's spelling into the WHERE clause is the regression it
// exists to prevent.
//
// The `id` tiebreak is for a DETERMINISTIC order only; the id itself is never
// selected and never leaves the server.
//
// The index that serves it is idx_go_ride_requests_vehicle_window (migration
// 0026), on the reservation arm, and it serves exactly three conjuncts:
// `r.vehicle_id = $1` on the leading column and the two `r.scheduled_for`
// bounds on the second — all three appear as Index Cond, so the scan touches
// the requested range instead of every open reservation on the car. The arm's
// `scheduled_for IS NOT NULL` and status conjuncts are the index's own partial
// predicate. The active-instant arm is a separate bitmap branch over the
// `scheduled_for IS NULL` partial indexes (0004/0013). No new migration — this
// is a read over existing columns.
var queryVehicleBookedWindows = `SELECT
	` + rideWindowStartExpr + ` AS window_start,
	` + rideWindowEndExpr + ` AS window_end,
	r.status = 'requested' AS pending,
	r.rider_id = $6 AS own
FROM go_ride_requests r
WHERE r.vehicle_id = $1
  AND ((
			` + rideWindowReservationOccupies + `
			AND ` + rideWindowOverlap(`r.scheduled_for`, `$2`, `$3`) + `
		) OR (
			` + rideWindowInstantOccupies + `
			AND ` + rideWindowOverlap(`NOW()`, `$2`, `$3`) + `
		))
ORDER BY window_start ASC, r.id ASC`

// ListBookedWindows returns every interval in [from, to) in which vehicleID is
// already spoken for, ordered by start. callerID decides the Own flag and
// NOTHING else — it is not an authorization term. Authorization is the HTTP
// layer's job and is the ride-CREATE capability check; this repo answers for
// whichever vehicle it is handed, exactly as the conflict probe does.
//
// An unknown vehicle, or a free one, is an empty (non-nil) slice, never an
// error: "no windows" is the ordinary answer and the common case, and a
// not-found here would leak the existence of cars the caller cannot see.
//
// The caller MUST have validated the range (from < to, span within
// MaxBookedWindowRange). A reversed range is not an error here — it simply
// matches nothing — because the repo has no way to report the distinction the
// HTTP layer needs, and duplicating validation at two levels is how the two
// come to disagree.
func (r *RideRequestRepo) ListBookedWindows(
	ctx context.Context, vehicleID, callerID string, from, to time.Time,
) ([]BookedWindow, error) {
	const op = "ride_request.list_booked_windows"
	start := time.Now()

	rows, err := r.pool.Query(ctx, queryVehicleBookedWindows,
		vehicleID, from, to, RideConflictWindow.Seconds(), bookedWindowsCountPending, callerID,
	)
	if err != nil {
		r.observeFailed(op, start)
		return nil, fmt.Errorf("RideRequestRepo.ListBookedWindows(%s): query: %w", vehicleID, err)
	}
	defer rows.Close()

	windows := make([]BookedWindow, 0)
	for rows.Next() {
		var w BookedWindow
		if err := rows.Scan(&w.Start, &w.End, &w.Pending, &w.Own); err != nil {
			r.observeFailed(op, start)
			return nil, fmt.Errorf("RideRequestRepo.ListBookedWindows(%s): scan: %w", vehicleID, err)
		}
		windows = append(windows, w)
	}
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError(op)
		return nil, fmt.Errorf("RideRequestRepo.ListBookedWindows(%s): rows: %w", vehicleID, err)
	}
	return windows, nil
}

// observeFailed records the duration and the error for one failed query, so the
// three failure returns above cannot each spell the pair differently.
func (r *RideRequestRepo) observeFailed(op string, start time.Time) {
	r.metrics.ObserveQueryDuration(op, time.Since(start).Seconds())
	r.metrics.IncQueryError(op)
}
