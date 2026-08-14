package arrival

// The arrival rules: pure functions and one tiny state machine, with no I/O,
// no bus and no clock of their own (every instant comes off the frame). They
// are separated from detector.go precisely so the decision — the part a false
// positive would come out of — can be tested exhaustively against scripted
// sequences without a database, a bus or a car.

import (
	"math"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// gearPark is Tesla's shift-state string for park, as normalised by the
// decoder (see internal/telemetry/converters_enums.go). The drive detector
// reads the same value from the same field.
const gearPark = "P"

// Fix is one observation of a car, distilled from a telemetry frame. Only
// Latitude/Longitude and At are required; everything else is nullable because
// a frame carries whatever the car chose to send in it.
//
// The coordinates are P1 GPS data — never logged.
type Fix struct {
	// At is the frame's own timestamp, which is what the dwell is measured on.
	// Using the frame clock rather than the server's means a burst of
	// backlogged frames delivered together cannot fake a dwell, and a REST
	// backfill frame (MYR-394, ~25s cadence) contributes its real elapsed time
	// rather than an arrival-order approximation.
	At        time.Time
	Latitude  float64
	Longitude float64
	// Speed is the car's speed in mph, nil when the frame carried none (or
	// carried it marked invalid).
	Speed *float64
	// Gear is the shift state, empty when the frame carried none.
	Gear string
	// MilesToArrival is the CAR'S OWN distance-to-destination. CORROBORATION
	// ONLY — it is logged when an arrival fires and is read by nothing else.
	// See the package doc: MYR-527 is the proof that the dash's target and the
	// ride's target are different facts.
	MilesToArrival *float64
}

// fixFrom distils a telemetry frame into a Fix. ok is false when the frame
// carries no usable position, which is most frames from a parked car and every
// frame from a car that is only reporting charge state — there is nothing to
// measure, so the track is not touched at all.
//
// The (0, 0) NO-FIX SENTINEL is rejected here (vehicle-state-schema.md §2.3).
// It is a real value in storage and on the wire, it decodes as a perfectly
// ordinary coordinate, and it is ~1,600 km off the Gulf of Guinea — so it can
// never be inside a real pickup's radius, but rejecting it explicitly means the
// intent is visible rather than an accident of geography.
func fixFrom(te events.VehicleTelemetryEvent) (Fix, bool) {
	loc := locationField(te.Fields)
	if loc == nil || (loc.Latitude == 0 && loc.Longitude == 0) {
		return Fix{}, false
	}
	return Fix{
		At:             te.CreatedAt,
		Latitude:       loc.Latitude,
		Longitude:      loc.Longitude,
		Speed:          floatField(te.Fields, telemetry.FieldSpeed),
		Gear:           stringField(te.Fields, telemetry.FieldGear),
		MilesToArrival: floatField(te.Fields, telemetry.FieldMilesToArrival),
	}, true
}

// locationField returns the frame's location, or nil when absent or marked
// invalid by the vehicle.
func locationField(fields map[string]events.TelemetryValue) *events.Location {
	v, ok := fields[string(telemetry.FieldLocation)]
	if !ok || v.Invalid {
		return nil
	}
	return v.LocationVal
}

// floatField returns a nullable float field, nil when absent or marked
// invalid. Nullable rather than (value, ok) because "the car did not say" has
// to survive all the way into the rules: absent speed is not zero speed.
func floatField(fields map[string]events.TelemetryValue, name telemetry.FieldName) *float64 {
	v, ok := fields[string(name)]
	if !ok || v.Invalid {
		return nil
	}
	return v.FloatVal
}

// stringField returns a string field, empty when absent or marked invalid.
func stringField(fields map[string]events.TelemetryValue, name telemetry.FieldName) string {
	v, ok := fields[string(name)]
	if !ok || v.Invalid || v.StringVal == nil {
		return ""
	}
	return *v.StringVal
}

// motion is what a fix's REPORTED fields say about whether the car is moving.
// Three-valued because the third value is the whole of MYR-563: a frame can
// decline to say, and "did not say" is not "said no".
type motion int

const (
	// motionUnreported — the frame carried neither a speed nor a gear. This is
	// the complete drive_state a parked car's REST backfill produces.
	motionUnreported motion = iota
	// motionStopped — the frame itself says the car is not moving.
	motionStopped
	// motionMoving — the frame itself says the car IS moving.
	motionMoving
)

// reportedMotion reads the two fields that speak for themselves. The ladder is
// unchanged from MYR-538 and the asymmetry is still deliberate:
//
//   - A reported speed decides on its own — above the threshold the car is
//     moving, at or below it the car is not. A car rolling at 3 mph through a
//     car park with gear P somehow set must not read as stopped, which is why
//     speed is checked FIRST and can refuse.
//   - With no speed reported, gear decides: P is stopped (a parked car is
//     stopped by definition), any other reported gear is moving.
//   - With neither, this function says NOTHING. It used to say "not stopped",
//     and that one cell of the truth table is the entire MYR-563 defect — see
//     stillSinceGiven for what now answers it.
func (f Fix) reportedMotion(maxSpeedMPH float64) motion {
	if f.Speed != nil {
		if *f.Speed <= maxSpeedMPH {
			return motionStopped
		}
		return motionMoving
	}
	switch f.Gear {
	case "":
		return motionUnreported
	case gearPark:
		return motionStopped
	default:
		return motionMoving
	}
}

// stopped reports whether this fix, on its own, is positive evidence that the
// car is not moving. Kept as the narrow question it always was — REPORTED
// stillness — because that is what the two decided rungs of the ladder answer.
func (f Fix) stopped(maxSpeedMPH float64) bool {
	return f.reportedMotion(maxSpeedMPH) == motionStopped
}

// stillSinceGiven answers "is this car stopped, and if so since when" for one
// fix in the context of the fix before it. The instant matters as much as the
// verdict: the positional rung below proves stillness across an INTERVAL, and
// throwing that away would make the dwell start over at every fix.
//
// THE THIRD RUNG, AND WHY IT IS EVIDENCE RATHER THAN A RELAXATION (MYR-563). A
// car that parks stops streaming — that is not an edge case, it is what Tesla
// does, and it happens at exactly the moment this detector exists to notice. For
// as long as the car then sits there, every frame is an MYR-394 REST backfill
// fix carrying a location and nothing else: Tesla answers speed=null and
// shift_state=null for a stationary vehicle, and addDriveStateFields skips both
// rather than inventing a zero or a "P". Requiring a reported speed or gear
// therefore did not make arrival detection conservative at a parked car, it made
// it impossible, and a prod ride sat at its stop for ten minutes proving it.
//
// So when the frame declines to say, the SEQUENCE is asked instead: two located
// fixes within StillnessMeters of each other are positive evidence that the car
// did not move between them. That is the same inference the drive detector
// already makes from the same frames (displacedEnough → no route point → no
// movement), and it is evidence in the strict sense the package doc demands —
// it can be false, and when it is false it says "moving".
//
// Two guards keep it honest:
//
//   - It NEVER outranks a reported field. Speed and gear are consulted first and
//     both can refuse, so a car crawling at 3 mph through a car park with fixes
//     40 m apart is still moving.
//   - It refuses to span a gap longer than MaxStillnessGap. Two identical
//     positions fifteen minutes apart are equally consistent with a car that
//     left and came back, and this detector does not guess. The run simply
//     restarts from the later fix, so a poll outage delays an arrival instead of
//     fabricating one.
func (f Fix) stillSinceGiven(prev *Fix, cfg Config) (time.Time, bool) {
	switch f.reportedMotion(cfg.StoppedSpeedMPH) {
	case motionStopped:
		// A reported stillness speaks for THIS instant only. One frame saying
		// "0 mph" says nothing about the second before it.
		return f.At, true
	case motionMoving:
		return time.Time{}, false
	case motionUnreported:
	}

	if prev == nil {
		return time.Time{}, false
	}
	gap := f.At.Sub(prev.At)
	if gap <= 0 || gap > cfg.MaxStillnessGap {
		return time.Time{}, false
	}
	if haversineMeters(prev.Latitude, prev.Longitude, f.Latitude, f.Longitude) > cfg.StillnessMeters {
		return time.Time{}, false
	}
	// Stillness proven across the interval, so the dwell may honestly count
	// from the EARLIER fix — the displacement test is a statement about both
	// ends of it, not just this one.
	return prev.At, true
}

// track is the per-ride dwell state. One per candidate ride, held in the
// detector's map. NOT safe for concurrent use and does not need to be: the bus
// guarantees serial delivery per subscription, so every track is touched from
// the one handler goroutine.
type track struct {
	// stillSince is the timestamp of the first frame in the CURRENT run of
	// qualifying frames, zero when the car is not currently qualifying. Any
	// disqualifying frame clears it, which is what makes an interrupted dwell
	// restart from scratch rather than resume.
	stillSince time.Time
	// prev is the last fix observed for this waypoint, held so the positional
	// stillness rung has something to compare against (MYR-563). It is updated
	// on EVERY observation, including ones that disqualify: a car outside the
	// radius still establishes where it was, and the next fix is entitled to
	// measure against it.
	prev *Fix
	// latched records that this ride's arrival has already been acted on, so
	// the ~20 further qualifying frames that arrive while the car sits there
	// do nothing. The exactly-once guarantee per (ride, waypoint).
	latched bool
	// verdict is the last state LOGGED for this waypoint, so the detector can
	// emit one line per transition instead of one per frame (MYR-563).
	verdict verdict
}

// observation is what one fix resolved to for one candidate: the answer, and
// the numbers a human reading the log needs to understand it. Returned rather
// than logged in here because rules.go holds no logger by design.
//
// distanceMeters is a DISTANCE, not a position — safe to log, unlike the
// coordinates it is derived from (P1).
type observation struct {
	distanceMeters float64
	inRadius       bool
	motion         motion
	// still reports whether the stillness ladder concluded the car is stopped.
	still bool
	// dwellMet is the arrival condition: inside the radius, still, for the
	// whole dwell window.
	dwellMet bool
}

// observe folds one fix into the track and reports what it resolved to. The
// arrival condition is observation.dwellMet: inside the radius, stopped, for the
// whole dwell.
//
// It is idempotent in the only way that matters — dwellMet is true on every
// qualifying frame once the dwell is complete, not just the first. Deciding
// WHICH of those becomes an arrival is the latch's job, not the rule's, so the
// rule stays a pure predicate over the sequence it has seen.
func (t *track) observe(f Fix, targetLat, targetLng float64, cfg Config) observation {
	prev := t.prev
	fix := f
	t.prev = &fix

	obs := observation{
		distanceMeters: haversineMeters(f.Latitude, f.Longitude, targetLat, targetLng),
		motion:         f.reportedMotion(cfg.StoppedSpeedMPH),
	}
	obs.inRadius = obs.distanceMeters <= cfg.RadiusMeters

	since, still := f.stillSinceGiven(prev, cfg)
	obs.still = still
	if !obs.inRadius || !still {
		t.stillSince = time.Time{}
		return obs
	}
	if t.stillSince.IsZero() {
		t.stillSince = since
	}
	// Not Before rather than After, so a zero dwell (tests) fires on the first
	// qualifying frame and a frame landing exactly on the boundary counts.
	obs.dwellMet = !f.At.Before(t.stillSince.Add(cfg.Dwell))
	return obs
}

// haversineMeters returns the great-circle distance in meters between two
// coordinates. 6_371_000 is the WGS-84 mean Earth radius in meters.
//
// Package-private and duplicated from internal/geocode and internal/store for
// the reason those two already state to each other: importing either would
// invert an established dependency direction to save nine lines of arithmetic.
func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusMeters = 6_371_000.0
	const deg2rad = math.Pi / 180.0

	dLat := (lat2 - lat1) * deg2rad
	dLng := (lng2 - lng1) * deg2rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*deg2rad)*math.Cos(lat2*deg2rad)*
			math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthRadiusMeters * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
