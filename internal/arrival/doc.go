// Package arrival detects, from the car's own telemetry, that a dispatched
// vehicle has reached its rider's pickup — and advances the ride
// `accepted → arrived` without waiting for the owner to tap "Picked up".
//
// THE CLIENT DIRECTIVE (MYR-538): "If car reaches destination you shouldn't
// require owner to mark as arrived we should be able to figure it out."
//
// # What it is allowed to conclude, and from what
//
// The whole detector is one sentence: a car with a LIVE dispatched ride
// (`accepted`, leg-1 `dispatch_status = 'sent'`) that is PARKED INSIDE the
// arrival radius of that ride's pickup, and has stayed that way for the DWELL
// window, has arrived. Nothing else is evidence.
//
// POSITION-VS-PICKUP IS PRIMARY, AND THE CAR'S OWN "MILES TO ARRIVAL" IS NOT
// EVIDENCE AT ALL. `milesToArrival` is the dash's distance to whatever the dash
// currently thinks the destination is, and MYR-527 is the standing proof that
// those can be different things: a rider spent three hours on a trip whose car
// was still navigating to the pickup, with the app faithfully rendering "4 min
// away" from the dash's own numbers. A detector that trusted that field would
// have marked the ride picked up in the rider's driveway on the way past. It is
// carried on the fix and written to the arrival log line as corroboration for
// whoever reads it later; it can neither trigger an arrival nor veto one.
//
// # Why it is deliberately slow to fire
//
// The two errors are not symmetric. A FALSE arrival wakes the rider's phone
// with "Your car is here", waves their Live Activity to the arrived phase, and
// tells them to walk outside to a car that is a block away and still moving —
// and none of that can be taken back, because the lifecycle has no
// arrived → accepted edge. A LATE arrival costs nothing at all: the owner's
// manual tap is still there, still works, and the rider is standing next to the
// car anyway. So every knob is set to the conservative side and every ambiguity
// resolves to "do nothing":
//
//   - an interrupted dwell RESETS (the car must be still for the WHOLE window,
//     not still on average),
//   - a frame with no location, or no evidence the car is stopped, is not
//     counted,
//   - a vehicle with more than one candidate ride is skipped entirely rather
//     than guessed at,
//   - a candidate snapshot that cannot be refreshed goes stale and then EMPTY,
//     so a database outage stops detection rather than running on old pickups,
//   - a refused write is never retried.
//
// # It is an ADDITIONAL WRITER, not a new path
//
// The transition goes through the SAME dormancy-guarded write the owner's tap
// uses (store.UpdateStatusFromDispatched, `WHERE status = ANY('accepted')` plus
// the MYR-376 predicate), so the owner tapping at the same instant is
// first-writer-wins arbitrated by the database exactly like every other
// transition race — and the loser is this detector, silently. The manual tap
// STAYS: it is the recovery path for every car that does not stream, every
// pickup whose coordinate is wrong, and every rider the car cannot get within
// 80 m of.
//
// The winning write publishes the SAME RideStatusChangedEvent shape the pickup
// handler publishes, so the rider's push, Live Activity and WS frame fire
// identically and no consumer needs to know which writer moved the row. It
// additionally publishes the internal ride.waypoint_arrived seam, which is the
// one thing only this path can say: the car is physically at the kerb.
//
// # Cost
//
// The detector runs on decoded telemetry frames — up to one per second per
// streaming car — so it never queries per frame. Candidates come from one lean
// indexed read, cached for ~15s (candidates.go), and every frame after that is
// a map lookup and a haversine.
package arrival
