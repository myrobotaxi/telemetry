package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// rideRequestStoreAdapter adapts store.RideRequestRepo to the
// telemetry.RideRequestStore interface used by the ride-request handlers
// (MYR-174/175). The adapter performs the row-shape + cursor translations at
// the boundary so the handlers stay decoupled from internal/store (same
// pattern as driveListerAdapter). The repo is the encrypt/decrypt boundary
// for pickup/dropoff coordinates — the adapter only moves plaintext values.
type rideRequestStoreAdapter struct {
	repo *store.RideRequestRepo
}

func (a *rideRequestStoreAdapter) Create(ctx context.Context, in telemetry.RideRequestCreateInput) (telemetry.RideRequestData, error) {
	rec, err := a.repo.Create(ctx, store.RideRequestRecord{
		RiderID:        in.RiderID,
		OwnerID:        in.OwnerID,
		VehicleID:      in.VehicleID,
		Pickup:         toStorePlace(in.Pickup),
		Dropoff:        toStorePlace(in.Dropoff),
		PassengerName:  in.PassengerName,
		PassengerPhone: in.PassengerPhone,
		ScheduledFor:   in.ScheduledFor,
	})
	if err != nil {
		// Translate the one-active-ride guard rejection into the handler-layer
		// sentinel (MYR-230), keeping the handler decoupled from internal/store.
		if errors.Is(err, store.ErrRideRequestActive) {
			return telemetry.RideRequestData{}, fmt.Errorf("create ride request: %w", telemetry.ErrRideActive)
		}
		return telemetry.RideRequestData{}, fmt.Errorf("create ride request: %w", err)
	}
	return fromStoreRideRequest(rec), nil
}

func (a *rideRequestStoreAdapter) GetByID(ctx context.Context, id string) (telemetry.RideRequestData, error) {
	rec, err := a.repo.GetByID(ctx, id)
	if err != nil {
		return telemetry.RideRequestData{}, fmt.Errorf("get ride request by id: %w", err)
	}
	return fromStoreRideRequest(rec), nil
}

// GetActiveInstantByRider passes through the rider's single OPEN instant ride
// (MYR-230). store.ErrRideRequestNotFound wraps sdk.ErrNotFound, so the
// handler's errors.Is(err, sdk.ErrNotFound) check sees "no open ride".
func (a *rideRequestStoreAdapter) GetActiveInstantByRider(ctx context.Context, riderID string) (telemetry.RideRequestData, error) {
	rec, err := a.repo.GetActiveInstantByRider(ctx, riderID)
	if err != nil {
		return telemetry.RideRequestData{}, fmt.Errorf("get active instant ride by rider: %w", err)
	}
	return fromStoreRideRequest(rec), nil
}

// repoStatusWriter is one of the repo's guarded transition writes — the plain
// MYR-174/175 guard or the MYR-376 dormancy-guarded one. Both adapter methods
// below funnel through guardedUpdate over this shape so the row and sentinel
// translations can never drift between them.
type repoStatusWriter func(ctx context.Context, id string, from []store.RideRequestStatus, to store.RideRequestStatus) (store.RideRequestRecord, error)

// UpdateStatusFrom delegates to the repo's guarded single-statement
// transition (MYR-174/175 race fix) and translates the store conflict
// sentinel into the handler layer's telemetry.ErrRideStatusConflict.
// sdk.ErrNotFound-wrapping errors pass through untouched.
func (a *rideRequestStoreAdapter) UpdateStatusFrom(ctx context.Context, id string, from []string, to string) (telemetry.RideRequestData, error) {
	return a.guardedUpdate(ctx, a.repo.UpdateStatusFrom, id, from, to)
}

// UpdateStatusFromDispatched delegates to the repo's DORMANCY-guarded variant
// (MYR-376) — the same single statement plus `AND (scheduled_for IS NULL OR
// dispatch_status = 'sent' OR scheduled_for <= NOW())` — and additionally
// translates store.ErrRideRequestReservationDormant into
// telemetry.ErrRideReservationDormant so the pickup handler can name the reason
// in its 409 without coupling to internal/store. Backs the owner pickup
// transition only.
func (a *rideRequestStoreAdapter) UpdateStatusFromDispatched(ctx context.Context, id string, from []string, to string) (telemetry.RideRequestData, error) {
	return a.guardedUpdate(ctx, a.repo.UpdateStatusFromDispatched, id, from, to)
}

// guardedUpdate runs one guarded transition write and translates the store's
// typed refusals into the handler layer's sentinels. Every sentinel is mapped
// here for BOTH variants: the dormancy one simply never fires for the plain
// guard, which cannot produce it.
func (a *rideRequestStoreAdapter) guardedUpdate(
	ctx context.Context, write repoStatusWriter, id string, from []string, to string,
) (telemetry.RideRequestData, error) {
	fromStatuses := make([]store.RideRequestStatus, 0, len(from))
	for _, s := range from {
		fromStatuses = append(fromStatuses, store.RideRequestStatus(s))
	}
	rec, err := write(ctx, id, fromStatuses, store.RideRequestStatus(to))
	if err != nil {
		if errors.Is(err, store.ErrRideRequestConflict) {
			return telemetry.RideRequestData{}, fmt.Errorf("update ride request status: %w", telemetry.ErrRideStatusConflict)
		}
		// Reservation dormancy (MYR-376): the ride is still legally `accepted`,
		// but it is neither dispatched nor yet due, so there is no pickup to
		// confirm. Same 409 class as the conflict above, different reason.
		if errors.Is(err, store.ErrRideRequestReservationDormant) {
			return telemetry.RideRequestData{}, fmt.Errorf("update ride request status: %w", telemetry.ErrRideReservationDormant)
		}
		// Per-vehicle one-active-ride guard (0013, MYR-266): the car is already
		// committed to another active ride. Translate to the handler sentinel so
		// the accept path can surface a 409 without coupling to internal/store.
		if errors.Is(err, store.ErrVehicleRideActive) {
			return telemetry.RideRequestData{}, fmt.Errorf("update ride request status: %w", telemetry.ErrVehicleRideActive)
		}
		return telemetry.RideRequestData{}, fmt.Errorf("update ride request status: %w", err)
	}
	return fromStoreRideRequest(rec), nil
}

func (a *rideRequestStoreAdapter) ListByRiderPage(ctx context.Context, riderID string, cursor telemetry.RideRequestListCursor, limit int) (telemetry.RideRequestListPage, error) {
	page, err := a.repo.ListByRiderPage(ctx, riderID, toStoreCursor(cursor), limit)
	if err != nil {
		return telemetry.RideRequestListPage{}, fmt.Errorf("list ride requests by rider: %w", err)
	}
	return fromStorePage(page), nil
}

func (a *rideRequestStoreAdapter) ListByOwnerPage(ctx context.Context, ownerID string, status *string, cursor telemetry.RideRequestListCursor, limit int) (telemetry.RideRequestListPage, error) {
	var storeStatus *store.RideRequestStatus
	if status != nil {
		s := store.RideRequestStatus(*status)
		storeStatus = &s
	}
	page, err := a.repo.ListByOwnerPage(ctx, ownerID, storeStatus, toStoreCursor(cursor), limit)
	if err != nil {
		return telemetry.RideRequestListPage{}, fmt.Errorf("list ride requests by owner: %w", err)
	}
	return fromStorePage(page), nil
}

// ListUpcomingByOwnerVehiclePage passes the MYR-360 upcoming-reservations
// slice through, translating the ASCENDING (scheduledFor, id) cursor at the
// boundary like every other cursor here.
func (a *rideRequestStoreAdapter) ListUpcomingByOwnerVehiclePage(ctx context.Context, ownerID, vehicleID string, cursor telemetry.RideRequestUpcomingCursor, limit int) (telemetry.RideRequestListPage, error) {
	page, err := a.repo.ListUpcomingByOwnerVehiclePage(ctx, ownerID, vehicleID, toStoreUpcomingCursor(cursor), limit)
	if err != nil {
		return telemetry.RideRequestListPage{}, fmt.Errorf("list upcoming ride reservations by owner vehicle: %w", err)
	}
	return fromStorePage(page), nil
}

// toStorePlace / toStoreCursor translate handler-layer inputs into store
// shapes; fromStoreRideRequest / fromStorePage translate the other way.

func toStorePlace(p telemetry.RidePlaceData) store.RidePlace {
	return store.RidePlace{
		Latitude:  p.Latitude,
		Longitude: p.Longitude,
		Label:     p.Label,
		Address:   p.Address,
	}
}

func toStoreCursor(c telemetry.RideRequestListCursor) store.RideRequestListCursor {
	return store.RideRequestListCursor{CreatedAt: c.CreatedAt, ID: c.ID}
}

func toStoreUpcomingCursor(c telemetry.RideRequestUpcomingCursor) store.RideRequestUpcomingCursor {
	return store.RideRequestUpcomingCursor{ScheduledFor: c.ScheduledFor, ID: c.ID}
}

func fromStorePlace(p store.RidePlace) telemetry.RidePlaceData {
	return telemetry.RidePlaceData{
		Latitude:  p.Latitude,
		Longitude: p.Longitude,
		Label:     p.Label,
		Address:   p.Address,
	}
}

func fromStoreRideRequest(rec store.RideRequestRecord) telemetry.RideRequestData {
	var reschedule *string
	if rec.RescheduleStatus != nil {
		s := string(*rec.RescheduleStatus)
		reschedule = &s
	}
	var dispatchStatus *string
	if rec.DispatchStatus != nil {
		s := string(*rec.DispatchStatus)
		dispatchStatus = &s
	}
	return telemetry.RideRequestData{
		ID:                    rec.ID,
		RiderID:               rec.RiderID,
		OwnerID:               rec.OwnerID,
		VehicleID:             rec.VehicleID,
		Pickup:                fromStorePlace(rec.Pickup),
		Dropoff:               fromStorePlace(rec.Dropoff),
		Status:                string(rec.Status),
		RequesterName:         optionalString(rec.RequesterName),
		PassengerName:         rec.PassengerName,
		PassengerPhone:        rec.PassengerPhone,
		ScheduledFor:          rec.ScheduledFor,
		RescheduleProposedFor: rec.RescheduleProposedFor,
		RescheduleStatus:      reschedule,
		AcceptedAt:            rec.AcceptedAt,
		CompletedAt:           rec.CompletedAt,
		CreatedAt:             rec.CreatedAt,
		UpdatedAt:             rec.UpdatedAt,
		DispatchStatus:        dispatchStatus,
		DispatchedAt:          rec.DispatchedAt,
		DispatchError:         rec.DispatchError,
	}
}

// optionalString maps the store layer's empty-string-means-absent convention
// (e.g. RequesterName, MYR-229) onto the handler layer's nil-pointer optional:
// "" -> nil (omitted on the wire), any other value -> &value.
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func fromStorePage(page store.RideRequestListPage) telemetry.RideRequestListPage {
	items := make([]telemetry.RideRequestData, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, fromStoreRideRequest(page.Items[i]))
	}
	return telemetry.RideRequestListPage{Items: items, HasMore: page.HasMore}
}
