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

// UpdateStatusFrom delegates to the repo's guarded single-statement
// transition (MYR-174/175 race fix) and translates the store conflict
// sentinel into the handler layer's telemetry.ErrRideStatusConflict.
// sdk.ErrNotFound-wrapping errors pass through untouched.
func (a *rideRequestStoreAdapter) UpdateStatusFrom(ctx context.Context, id string, from []string, to string) (telemetry.RideRequestData, error) {
	fromStatuses := make([]store.RideRequestStatus, 0, len(from))
	for _, s := range from {
		fromStatuses = append(fromStatuses, store.RideRequestStatus(s))
	}
	rec, err := a.repo.UpdateStatusFrom(ctx, id, fromStatuses, store.RideRequestStatus(to))
	if err != nil {
		if errors.Is(err, store.ErrRideRequestConflict) {
			return telemetry.RideRequestData{}, fmt.Errorf("update ride request status: %w", telemetry.ErrRideStatusConflict)
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
	return telemetry.RideRequestData{
		ID:                    rec.ID,
		RiderID:               rec.RiderID,
		OwnerID:               rec.OwnerID,
		VehicleID:             rec.VehicleID,
		Pickup:                fromStorePlace(rec.Pickup),
		Dropoff:               fromStorePlace(rec.Dropoff),
		Status:                string(rec.Status),
		PassengerName:         rec.PassengerName,
		PassengerPhone:        rec.PassengerPhone,
		ScheduledFor:          rec.ScheduledFor,
		RescheduleProposedFor: rec.RescheduleProposedFor,
		RescheduleStatus:      reschedule,
		AcceptedAt:            rec.AcceptedAt,
		CompletedAt:           rec.CompletedAt,
		CreatedAt:             rec.CreatedAt,
		UpdatedAt:             rec.UpdatedAt,
	}
}

func fromStorePage(page store.RideRequestListPage) telemetry.RideRequestListPage {
	items := make([]telemetry.RideRequestData, 0, len(page.Items))
	for i := range page.Items {
		items = append(items, fromStoreRideRequest(page.Items[i]))
	}
	return telemetry.RideRequestListPage{Items: items, HasMore: page.HasMore}
}
