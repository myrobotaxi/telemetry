package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// The classification in isArrivalRefusal is the boundary the owner-tap race
// resolves at: everything it calls a refusal makes the detector go silent and
// stay silent, and everything else makes it retry on a later frame. Getting a
// transport error onto the refusal side would silently disable auto-arrival for
// that ride; getting a lost race onto the retry side would put a doomed UPDATE
// on every frame for as long as the car sat at the kerb.
func TestIsArrivalRefusal(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "status left the allowed-from set — the owner tapped Picked up first",
			err:  fmt.Errorf("RideRequestRepo.UpdateStatusFromDispatched(x): %w", store.ErrRideRequestConflict),
			want: true,
		},
		{
			name: "the row is gone",
			err:  fmt.Errorf("RideRequestRepo.UpdateStatusFromDispatched(x): %w", store.ErrRideRequestNotFound),
			want: true,
		},
		{
			name: "dormant reservation (unreachable for a dispatched candidate, still a refusal)",
			err:  fmt.Errorf("RideRequestRepo.UpdateStatusFromDispatched(x): %w", store.ErrRideRequestReservationDormant),
			want: true,
		},
		{
			name: "the vehicle is committed to another active ride",
			err:  fmt.Errorf("RideRequestRepo.UpdateStatusFromDispatched(x): %w", store.ErrVehicleRideActive),
			want: true,
		},
		{
			name: "a database failure says nothing about the ride and must be retryable",
			err:  errors.New("conn busy: connection reset by peer"),
			want: false,
		},
		{
			name: "a context deadline is likewise retryable",
			err:  fmt.Errorf("query: %w", context.DeadlineExceeded),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isArrivalRefusal(tt.err); got != tt.want {
				t.Errorf("isArrivalRefusal(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
