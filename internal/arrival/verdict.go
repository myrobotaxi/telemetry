// THE DETECTOR'S ACCOUNT OF ITSELF (MYR-563).
//
// Three field investigations in a row have ended at the same wall: a car did
// not advance, every component's tests pass, and `flyctl logs` has nothing at
// all to say about the decision — because until now this package logged only
// the two outcomes that WROTE something. A detector whose entire job is to
// decide "not yet" twenty times a second, and which says nothing when it decides
// it, is undiagnosable by construction. The last investigation had to be run
// against the production database and a screenshot of a car's dashboard.
//
// So every candidate now narrates its state, under two rules that are what make
// this affordable on a path that runs once per second per streaming car:
//
//	TRANSITIONS ONLY. A line is emitted when a waypoint's verdict CHANGES, never
//	per frame. A car sitting parked at a stop for ten minutes produces the four
//	lines of its story (armed, entered radius, dwell satisfied, published) and
//	then nothing, however many hundreds of frames arrive. The steady state — the
//	overwhelming majority of frames, from cars nowhere near a waypoint — is one
//	line per waypoint and then silence.
//
//	IDS AND DISTANCES, NEVER POSITIONS. A distance to a target is not a location:
//	it is one scalar that cannot be inverted into a coordinate without already
//	knowing the target. Coordinates are P1 and appear nowhere here, VINs are
//	redacted to their last four (CLAUDE.md), and the waypoint label carries a
//	stop id, which is a server-owned identifier and not a place.

package arrival

import (
	"log/slog"
	"sort"
	"strings"
)

// verdict is one state in a candidate's story. The zero value is deliberately
// "nothing said yet", so the first real verdict is always a transition and
// always logged.
type verdict string

const (
	verdictNone verdict = ""
	// verdictArmed — a (ride, waypoint) pair is now being watched.
	verdictArmed verdict = "armed"
	// verdictOutsideRadius — located, but too far from the target to matter.
	verdictOutsideRadius verdict = "suppressed:outside_radius"
	// verdictMoving — inside the radius, but the car is reported moving.
	verdictMoving verdict = "suppressed:moving"
	// verdictMotionUnknown — inside the radius and the frame says nothing about
	// motion, with no previous fix close enough in time to settle it. This is
	// the MYR-563 state: before the positional rung existed it was where a
	// parked car stayed forever, so it is named rather than lumped in with
	// "moving".
	verdictMotionUnknown verdict = "suppressed:motion_unknown"
	// verdictDwelling — inside the radius and still, dwell accumulating.
	verdictDwelling verdict = "entered_radius"
	// verdictDwellSatisfied — the arrival condition is met; a write or a
	// publish is about to be attempted.
	verdictDwellSatisfied verdict = "dwell_satisfied"
	// verdictPublished — the arrival went out.
	verdictPublished verdict = "event_published"
	// verdictSuppressedRefused — the guarded write matched no row. Ordinary:
	// somebody else settled this ride.
	verdictSuppressedRefused verdict = "suppressed:write_refused"
	// verdictSuppressedUnavailable — the write could not be completed, so the
	// latch was released and a later frame may try again.
	verdictSuppressedUnavailable verdict = "suppressed:write_unavailable"
)

// verdict maps one observation onto the state it represents. Only called for
// observations that did NOT meet the dwell — the caller names the rest, because
// what happens after a satisfied dwell depends on the write, not on the fix.
func (o observation) verdict() verdict {
	switch {
	case !o.inRadius:
		return verdictOutsideRadius
	case o.still:
		return verdictDwelling
	case o.motion == motionMoving:
		return verdictMoving
	default:
		return verdictMotionUnknown
	}
}

// noteVerdict logs a candidate's state IF it changed, and records it. This is
// the whole rate-safety story: the comparison against the previous verdict is
// what turns a per-frame path into a per-transition one.
//
// Level follows consequence rather than sentiment. The states on the way to an
// arrival are Info, because they are the trail a field report is reconstructed
// from and there are at most a handful per ride. The two write-outcome
// suppressions are Info as well for the same reason — reaching them means a
// dwell completed and produced nothing, which is exactly what somebody will
// come looking for.
func (d *Detector) noteVerdict(cand Candidate, tr *track, v verdict, obs observation) {
	if v == verdictNone || tr.verdict == v {
		return
	}
	prev := tr.verdict
	tr.verdict = v

	attrs := []any{
		slog.String("verdict", string(v)),
		slog.String("ride_id", cand.RideRequestID),
		slog.String("waypoint", cand.Waypoint),
		slog.String("vin", redactVIN(cand.VIN)),
	}
	if prev != verdictNone {
		attrs = append(attrs, slog.String("previous_verdict", string(prev)))
	}
	// The distance is the number every one of these investigations wanted and
	// none of them had. Omitted only for `armed`, which is emitted before any
	// fix has been measured.
	if v != verdictArmed {
		attrs = append(attrs,
			slog.Float64("distance_meters", round1(obs.distanceMeters)),
			slog.Float64("radius_meters", d.cfg.RadiusMeters),
		)
	}
	d.logger.Info("auto-arrival: "+verdictMessage(v), attrs...)
}

// verdictMessage renders the human half of the line. Written as sentences an
// operator can grep for and read at 2am, not as a restatement of the constant.
func verdictMessage(v verdict) string {
	switch v {
	case verdictArmed:
		return "watching a waypoint"
	case verdictOutsideRadius:
		return "car is not at the waypoint"
	case verdictMoving:
		return "car is inside the radius but still moving"
	case verdictMotionUnknown:
		return "car is inside the radius but nothing proves it is stopped"
	case verdictDwelling:
		return "car is stopped inside the radius, dwell running"
	case verdictDwellSatisfied:
		return "dwell satisfied, car has reached the waypoint"
	case verdictPublished:
		return "arrival published"
	case verdictSuppressedRefused:
		return "arrival not written, another writer settled the ride"
	case verdictSuppressedUnavailable:
		return "arrival could not be written, will retry on a later fix"
	default:
		return string(v)
	}
}

// round1 trims a distance to one decimal. A metre of precision is more than the
// question ever needs, and an unrounded float64 makes a log line unreadable.
func round1(m float64) float64 { return float64(int64(m*10+0.5)) / 10 }

// candidateSetFingerprint renders a snapshot as the sorted list of the
// (ride, waypoint) pairs it watches, so a rebuild can be logged only when the
// set actually CHANGED rather than every time the TTL expires.
//
// Which is the difference between a useful line and noise: the refresh happens
// every ~15s for as long as any car is streaming, forever, while the set itself
// changes only when a ride starts, ends, or its leg advances — a handful of
// times per ride. It is also the line that answers "did the detector notice my
// trip edit", which is the question MYR-563 was filed as.
func candidateSetFingerprint(byVIN map[string]Candidate) string {
	if len(byVIN) == 0 {
		return ""
	}
	keys := make([]string, 0, len(byVIN))
	for _, c := range byVIN {
		keys = append(keys, c.trackKey())
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
