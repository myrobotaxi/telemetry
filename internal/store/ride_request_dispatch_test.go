package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// TestRideRequestRepo_ClaimDispatch_ExactlyOnce verifies the dispatch latch:
// the first claim wins, a re-delivered event loses, and the win stamps
// dispatched_at while leaving dispatch_status unresolved (nil).
func TestRideRequestRepo_ClaimDispatch_ExactlyOnce(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, fullRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	claimed, err := repo.ClaimDispatch(ctx, created.ID)
	if err != nil {
		t.Fatalf("ClaimDispatch #1: %v", err)
	}
	if !claimed {
		t.Fatal("first ClaimDispatch = false, want true (won the claim)")
	}

	again, err := repo.ClaimDispatch(ctx, created.ID)
	if err != nil {
		t.Fatalf("ClaimDispatch #2: %v", err)
	}
	if again {
		t.Fatal("second ClaimDispatch = true, want false (already claimed)")
	}

	got, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.DispatchedAt == nil {
		t.Error("DispatchedAt nil after claim, want stamped")
	}
	if got.DispatchStatus != nil {
		t.Errorf("DispatchStatus = %v after claim, want nil (unresolved)", *got.DispatchStatus)
	}
}

func TestRideRequestRepo_ClaimDispatch_UnknownID(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	claimed, err := repo.ClaimDispatch(context.Background(), "cnope000000000000000000000")
	if err != nil {
		t.Fatalf("ClaimDispatch unknown id: %v", err)
	}
	if claimed {
		t.Error("ClaimDispatch on unknown id = true, want false")
	}
}

func TestRideRequestRepo_RecordDispatchOutcome(t *testing.T) {
	tests := []struct {
		name    string
		status  store.DispatchStatus
		errCode *string
	}{
		{"sent", store.DispatchStatusSent, nil},
		{"skipped", store.DispatchStatusSkipped, nil},
		{"failed with code", store.DispatchStatusFailed, strPtr("key_not_paired")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _ := setupRideRequestRepo(t)
			ctx := context.Background()
			created, err := repo.Create(ctx, fullRideRequest())
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, err := repo.ClaimDispatch(ctx, created.ID); err != nil {
				t.Fatalf("ClaimDispatch: %v", err)
			}
			if err := repo.RecordDispatchOutcome(ctx, created.ID, tt.status, tt.errCode); err != nil {
				t.Fatalf("RecordDispatchOutcome: %v", err)
			}

			got, err := repo.GetByID(ctx, created.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if got.DispatchStatus == nil || *got.DispatchStatus != tt.status {
				t.Errorf("DispatchStatus = %v, want %s", got.DispatchStatus, tt.status)
			}
			switch {
			case tt.errCode == nil && got.DispatchError != nil:
				t.Errorf("DispatchError = %v, want nil", *got.DispatchError)
			case tt.errCode != nil && (got.DispatchError == nil || *got.DispatchError != *tt.errCode):
				t.Errorf("DispatchError = %v, want %q", got.DispatchError, *tt.errCode)
			}
		})
	}
}

func TestRideRequestRepo_RecordDispatchOutcome_UnknownID(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	err := repo.RecordDispatchOutcome(context.Background(), "cnope000000000000000000000", store.DispatchStatusSent, nil)
	if !errors.Is(err, store.ErrRideRequestNotFound) {
		t.Errorf("RecordDispatchOutcome unknown id err = %v, want ErrRideRequestNotFound", err)
	}
}
