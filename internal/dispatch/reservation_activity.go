package dispatch

import (
	"context"
	"log/slog"
)

// The Live Activity seam on reservation expiry (MYR-172).
//
// Every other lifecycle ending in this service publishes `ride.status.changed`,
// and the Live Activity consumer subscribes there — so accept, decline, cancel,
// pickup, start and dropoff are all covered without a single call site being
// edited. Reservation expiry is the exception, and deliberately so: when a
// scheduled ride runs past its lateness ceiling the sweeper records a DISPATCH
// failure and leaves the ride row at `accepted`, because the ride was never
// declined and nobody cancelled it — the car simply never went.
//
// That is the right thing for the ride record and the wrong thing for a lock
// screen. Without this seam, the rider of a reservation that quietly failed
// would be left with a Live Activity promising a pickup, counting down to an
// ETA that will never arrive, until the 24-hour sweep removed the registration.
// This is the one place the Activity has to be told something the event bus
// does not carry.

// RideActivityEnder ends the Live Activities running on a ride.
//
// Declared here at the consumer site, satisfied by *push.ActivityNotifier, so
// internal/dispatch never imports internal/push.
type RideActivityEnder interface {
	EndForReservationExpiry(ctx context.Context, rideRequestID string)
}

// WithActivityEnder wires the Live Activity seam. Nil leaves it a no-op, which
// is the pre-MYR-172 behaviour and the state a deployment without the notifier
// wired runs in.
func (s *ReservationSweeper) WithActivityEnder(ender RideActivityEnder) *ReservationSweeper {
	s.activities = ender
	return s
}

// endActivities tells the rider's Live Activity the reservation is over.
//
// Fire-and-forget and drop-safe, exactly like publishDue beside it: the
// dispatch failure is the contract, and the Activity update is a notification
// hook. A missing ender is silent — it is the ordinary unwired state, not a
// fault — and the notifier logs its own failures, so nothing is swallowed here.
func (s *ReservationSweeper) endActivities(ctx context.Context, rideRequestID string) {
	if s.activities == nil {
		return
	}
	s.logger.Info("reservation sweep: ending live activities for an expired reservation",
		slog.String("ride_id", rideRequestID),
	)
	s.activities.EndForReservationExpiry(ctx, rideRequestID)
}
