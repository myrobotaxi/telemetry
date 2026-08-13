package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/arrival"
	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// Auto-arrival wiring (MYR-538). Composes the detector that watches decoded
// telemetry frames and advances a ride to `arrived` when its car is observed
// parked at the pickup — the client's "if car reaches destination you shouldn't
// require owner to mark as arrived".
//
// Lives at cmd/ for the same dependency-rule reason as the reservation sweeper:
// internal/arrival depends only on two small consumer-site seams, adapted here
// onto the ride-request repo. cmd/ is also the only layer that can see both
// internal/store and internal/arrival, which is what lets the adapter reuse the
// EXACT write the owner's HTTP tap uses rather than a second one shaped like
// it.

// startAutoArrivalDetector builds and starts the detector, or does nothing when
// AUTO_ARRIVAL_ENABLED is false. Returns the detector so the caller can Stop it
// on shutdown; nil when the switch is off.
//
// Started AFTER the nav dispatcher and the ride position poller: the detector
// only ever considers rides whose leg-1 dispatch already resolved `sent`, so
// the dispatch reconcile should have settled the rides orphaned by the previous
// process before the first candidate read runs, and a car that is not streaming
// needs the MYR-394 poller running to produce any frame at all.
func startAutoArrivalDetector(
	ctx context.Context,
	cfg *config.Config,
	bus events.Bus,
	rideRepo *store.RideRequestRepo,
	logger *slog.Logger,
) (*arrival.Detector, error) {
	if !cfg.AutoArrivalEnabled() {
		logger.Info("auto-arrival disabled by AUTO_ARRIVAL_ENABLED; " +
			"a ride reaches arrived only on the owner's Picked up tap")
		return nil, nil //nolint:nilnil // "off" is not an error and there is no detector to return
	}

	arrivalCfg := arrival.DefaultConfig()
	detector := arrival.NewDetector(
		bus,
		&arrivalStoreAdapter{repo: rideRepo},
		&arrivalStoreAdapter{repo: rideRepo},
		arrivalCfg,
		logger.With(slog.String("component", "auto-arrival")),
	)
	if err := detector.Start(ctx); err != nil {
		return nil, fmt.Errorf("starting auto-arrival detector: %w", err)
	}
	return detector, nil
}

// arrivalStoreAdapter adapts the ride-request repo to BOTH of the detector's
// seams — the candidate read and the arrival write.
//
// One type for two interfaces on purpose: they are two halves of one
// conversation with one repo, and splitting them would only invite a future
// edit to point the write at a different table from the read.
type arrivalStoreAdapter struct {
	repo *store.RideRequestRepo
}

// ListArrivalCandidates projects the repo's lean rows onto the detector's type.
// This is where the P1 target coordinates cross into the arrival package —
// already decrypted by the repo's scan, exactly as the reservation adapter
// hands the sweeper its pickup. The waypoint label is carried across untouched:
// the SQL chose it alongside the coordinate, and re-deriving it here is how the
// two would come to disagree.
func (a *arrivalStoreAdapter) ListArrivalCandidates(ctx context.Context, limit int) ([]arrival.Candidate, error) {
	rows, err := a.repo.ListArrivalCandidates(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]arrival.Candidate, 0, len(rows))
	// Indexed rather than ranged by value, matching the reservation adapter:
	// the row is wide enough that gocritic flags the per-iteration copy.
	for i := range rows {
		r := &rows[i]
		out = append(out, arrival.Candidate{
			RideRequestID:   r.RideRequestID,
			VehicleID:       r.VehicleID,
			RiderID:         r.RiderID,
			OwnerID:         r.OwnerID,
			VIN:             r.VIN,
			Waypoint:        r.Waypoint,
			TargetLatitude:  r.TargetLatitude,
			TargetLongitude: r.TargetLongitude,
		})
	}
	return out, nil
}

// MarkArrived advances the ride accepted → arrived through
// UpdateStatusFromDispatched — THE SAME dormancy-guarded single UPDATE the
// owner's `POST /api/ride-requests/{id}/picked-up` runs, with the same
// allowed-from set.
//
// That identity is the whole race argument, and it is why this adapter method
// exists instead of a new repo write. Detector and owner tap contend on one
// statement, the database picks a winner, and the loser gets a zero-row miss.
// The manual tap is untouched by any of this — it keeps its 409, its message
// and its idempotent already-`arrived` 200.
//
// The MYR-376 dormancy predicate rides along and costs nothing here: a
// candidate is only ever a ride whose leg-1 dispatch resolved `sent`, which is
// one of the predicate's own escape hatches. It is kept anyway rather than
// switched to the plain guard, because the value of "there is exactly one way
// to write this transition" is worth more than a conjunct that is always true.
//
// Every refusal — a lost race, a cancel, a vanished row — collapses to
// arrival.ErrRefused, which the detector treats as final and silent. Only a
// TRANSPORT failure keeps its own error, because only that one says nothing
// about the ride and may be retried.
//
// The returned event is built exactly as the pickup handler's mutateStatusWith
// builds it, from the UPDATED row, so no downstream consumer can tell the two
// writers apart. PreviousStatus is stamped by the detector.
func (a *arrivalStoreAdapter) MarkArrived(ctx context.Context, rideID string) (events.RideStatusChangedEvent, error) {
	updated, err := a.repo.UpdateStatusFromDispatched(
		ctx, rideID,
		[]store.RideRequestStatus{store.RideRequestStatusAccepted},
		store.RideRequestStatusArrived,
	)
	if err != nil {
		if isArrivalRefusal(err) {
			return events.RideStatusChangedEvent{}, fmt.Errorf("auto-arrival %s: %w", rideID, arrival.ErrRefused)
		}
		return events.RideStatusChangedEvent{}, fmt.Errorf("auto-arrival %s: %w", rideID, err)
	}
	// The same two optional projections fromStoreRideRequest performs, so the
	// event this publishes is field-for-field the one the HTTP path publishes:
	// the store's empty-string-means-absent requester name becomes a nil
	// pointer, and the typed reschedule sub-state becomes a plain string.
	var reschedule *string
	if updated.RescheduleStatus != nil {
		s := string(*updated.RescheduleStatus)
		reschedule = &s
	}
	return events.RideStatusChangedEvent{
		RideRequestID:    updated.ID,
		VehicleID:        updated.VehicleID,
		RiderID:          updated.RiderID,
		OwnerID:          updated.OwnerID,
		Status:           string(updated.Status),
		RequesterName:    optionalString(updated.RequesterName),
		RescheduleStatus: reschedule,
		ScheduledFor:     updated.ScheduledFor,
		CancelledBy:      updated.CancelledBy,
		UpdatedAt:        updated.UpdatedAt,
	}, nil
}

// isArrivalRefusal reports whether a guarded-write failure means "somebody else
// already decided this ride's status" rather than "the database is unwell".
//
// All four are ordinary outcomes of an unattended writer racing a person:
// the status left the allowed-from set (the owner's tap, or a cancel), the row
// is gone, the vehicle picked up another ride, or — unreachable for a `sent`
// candidate, listed for completeness — the reservation reads as dormant.
func isArrivalRefusal(err error) bool {
	return errors.Is(err, store.ErrRideRequestConflict) ||
		errors.Is(err, store.ErrRideRequestNotFound) ||
		errors.Is(err, store.ErrRideRequestReservationDormant) ||
		errors.Is(err, store.ErrVehicleRideActive)
}
