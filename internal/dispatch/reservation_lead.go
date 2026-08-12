// The dispatch lead (MYR-535): how early before `scheduledFor` a reserved
// car is sent its pickup nav.
//
// `scheduledFor` is the instant the rider expects the car to ARRIVE. Before
// this file the sweeper dispatched AT that instant, so the car left when it
// should have been pulling up — measured in prod on ride c3dbd5e6…:
// scheduled 21:00:00Z, dispatched 21:00:08Z, and the client's own words for
// it: "But like the owner should leave before 4PM."
//
// The client decision (2026-08-12): the server estimates drive time from the
// car's CURRENT position to the pickup and dispatches that early, plus a small
// buffer, floored and capped — see ReservationConfig.LeadBuffer/Floor/Cap.
//
// The estimate is the platform's ONE closed form: great-circle distance
// × 1.3 route factor ÷ 24 mph. Deliberately the same arithmetic as the iOS
// client's TripEstimate (and the same trade: a city-hop average OVERSTATES
// highway drive time, which here errs toward leaving early — bounded by
// LeadCap — never late). Not a routing API: the sweeper re-decides every tick
// for every candidate in the window, which is exactly the visible-cadence
// re-ask a routing provider throttles, and a wrong estimate can only move
// WHEN the car leaves within [LeadFloor, LeadCap].

package dispatch

import (
	"context"
	"log/slog"
	"math"
	"time"
)

const (
	// leadRouteFactor inflates great-circle distance toward road distance.
	leadRouteFactor = 1.3
	// leadAvgSpeedMph converts road miles to minutes. TripEstimate parity.
	leadAvgSpeedMph = 24.0
	// earthRadiusMiles for the haversine.
	earthRadiusMiles = 3958.8
)

// travelEstimate returns the estimated drive time between two coordinates
// via the closed form above. Pure.
func travelEstimate(fromLat, fromLng, toLat, toLng float64) time.Duration {
	miles := haversineMiles(fromLat, fromLng, toLat, toLng) * leadRouteFactor
	hours := miles / leadAvgSpeedMph
	return time.Duration(hours * float64(time.Hour))
}

// haversineMiles is the great-circle distance between two coordinates.
func haversineMiles(lat1, lng1, lat2, lng2 float64) float64 {
	const degToRad = math.Pi / 180
	dLat := (lat2 - lat1) * degToRad
	dLng := (lng2 - lng1) * degToRad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*degToRad)*math.Cos(lat2*degToRad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusMiles * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// clampLead applies buffer, floor and cap to a raw travel estimate. Pure.
func clampLead(travel time.Duration, cfg ReservationConfig) time.Duration {
	lead := travel + cfg.LeadBuffer
	if lead < cfg.LeadFloor {
		return cfg.LeadFloor
	}
	if lead > cfg.LeadCap {
		return cfg.LeadCap
	}
	return lead
}

// hasFix reports whether a decrypted position pair is usable for the
// estimate. The nil pair is "the server holds no position"; (0, 0) is the
// wire's NO-FIX SENTINEL (vehicle-state-schema.md §2.3), which storage does
// not collapse, so it is refused here — an estimate from Null Island would
// put every real pickup at the LeadCap.
func hasFix(lat, lng *float64) bool {
	if lat == nil || lng == nil {
		return false
	}
	return *lat != 0 || *lng != 0
}

// holdIfBeforeLeaveTime is the sweeper's early gate (MYR-535). A candidate
// whose scheduledFor is still in the future is dispatched only once
// now >= scheduledFor - lead; before that it is WAITING — nothing is claimed,
// nothing is logged per row (a candidate can wait up to LeadCap across ~60
// ticks, and 60 identical log lines per reservation say nothing), and the next
// tick re-decides with the car's newer position.
//
// The gate runs BEFORE the busy/blocked/grant probes, so a waiting candidate
// costs one lean position read per tick and no more.
//
// A candidate at or past scheduledFor never reaches the position read at all:
// the lead only decides how EARLY to leave, and once the pickup instant has
// arrived the answer is "now" whatever the estimate says — so a position-read
// outage can delay a dispatch by at most the lead it would have granted, and
// can never hold one past its pickup instant.
//
// Degradations both land on the FLOOR lead, deliberately asymmetric with the
// other probes' hold-on-error posture: those guard an irreversible claim
// against dispatching a car that MUST NOT go; this one only tunes when a car
// that definitely should go actually leaves. A car with no usable fix (never
// streamed, or asleep since before the encrypted backfill) still leaves
// LeadFloor early; an unreadable position row is logged and treated the same.
func (s *ReservationSweeper) holdIfBeforeLeaveTime(ctx context.Context, r *DueReservation, now time.Time) bool {
	if !now.Before(r.ScheduledFor) {
		return false
	}
	lead := s.cfg.LeadFloor
	lat, lng, err := s.store.VehiclePosition(ctx, r.VehicleID)
	switch {
	case err != nil:
		s.logger.Warn("reservation sweep: vehicle position read failed, using floor lead",
			slog.String("ride_id", r.RideRequestID),
			slog.String("vehicle_id", r.VehicleID),
			slog.String("error", err.Error()),
		)
	case hasFix(lat, lng):
		lead = clampLead(travelEstimate(*lat, *lng, r.Pickup.Latitude, r.Pickup.Longitude), s.cfg)
	}
	return now.Before(r.ScheduledFor.Add(-lead))
}
