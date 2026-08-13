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

// stopped reports whether this fix is POSITIVE EVIDENCE that the car is not
// moving. The asymmetry is deliberate: silence is not stillness.
//
//   - A reported speed decides on its own — above the threshold the car is
//     moving, at or below it the car is not. A car rolling at 3 mph through a
//     car park with gear P somehow set must not read as stopped, which is why
//     speed is checked FIRST and can refuse.
//   - With no speed reported, gear P is the fallback: a parked car is stopped
//     by definition, and a car that has just been put in park at the kerb is
//     the exact situation this feature exists for.
//   - With neither, the answer is no. A frame carrying only a location tells us
//     where the car is and nothing about whether it is still there in a second.
func (f Fix) stopped(maxSpeedMPH float64) bool {
	if f.Speed != nil {
		return *f.Speed <= maxSpeedMPH
	}
	return f.Gear == gearPark
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
	// latched records that this ride's arrival has already been acted on, so
	// the ~20 further qualifying frames that arrive while the car sits there
	// do nothing. The exactly-once guarantee per (ride, waypoint).
	latched bool
}

// observe folds one fix into the track and reports whether the arrival
// condition has just been met: inside the radius, stopped, for the whole dwell.
//
// It is idempotent in the only way that matters — it reports true on every
// qualifying frame once the dwell is complete, not just the first. Deciding
// WHICH of those becomes an arrival is the latch's job, not the rule's, so the
// rule stays a pure predicate over the sequence it has seen.
func (t *track) observe(f Fix, pickupLat, pickupLng float64, cfg Config) bool {
	near := haversineMeters(f.Latitude, f.Longitude, pickupLat, pickupLng) <= cfg.RadiusMeters
	if !near || !f.stopped(cfg.StoppedSpeedMPH) {
		t.stillSince = time.Time{}
		return false
	}
	if t.stillSince.IsZero() {
		t.stillSince = f.At
	}
	// Not Before rather than After, so a zero dwell (tests) fires on the first
	// qualifying frame and a frame landing exactly on the boundary counts.
	return !f.At.Before(t.stillSince.Add(cfg.Dwell))
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
