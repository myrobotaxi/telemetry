package store

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
)

// stubVINLookup is a test double for vinLookup that returns configurable
// results and counts calls.
type stubVINLookup struct {
	vehicles map[string]Vehicle // VIN → Vehicle
	err      error              // returned for all lookups if set
	calls    atomic.Int64
}

func (s *stubVINLookup) GetByVIN(_ context.Context, vin string) (Vehicle, error) {
	s.calls.Add(1)
	if s.err != nil {
		return Vehicle{}, s.err
	}
	v, ok := s.vehicles[vin]
	if !ok {
		return Vehicle{}, ErrVehicleNotFound
	}
	return v, nil
}

func TestVINCache_Resolve(t *testing.T) {
	logger := slog.Default()

	tests := []struct {
		name      string
		vehicles  map[string]Vehicle
		lookupErr error
		vin       string
		wantID    string
		wantErr   error
	}{
		{
			name: "cache miss then hit",
			vehicles: map[string]Vehicle{
				"5YJ3E1EA1NF000001": {ID: "veh_001", VIN: "5YJ3E1EA1NF000001"},
			},
			vin:    "5YJ3E1EA1NF000001",
			wantID: "veh_001",
		},
		{
			name:     "vehicle not found cached",
			vehicles: map[string]Vehicle{},
			vin:      "UNKNOWN",
			wantErr:  ErrVehicleNotFound,
		},
		{
			name:      "transient error not cached",
			vehicles:  map[string]Vehicle{},
			lookupErr: errors.New("connection refused"),
			vin:       "5YJ3E1EA1NF000001",
			wantErr:   errors.New("connection refused"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := &stubVINLookup{vehicles: tt.vehicles, err: tt.lookupErr}
			cache := newVINCache(lookup, logger)
			ctx := context.Background()

			id, err := cache.resolve(ctx, tt.vin)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if errors.Is(tt.wantErr, ErrVehicleNotFound) && !errors.Is(err, ErrVehicleNotFound) {
					t.Fatalf("expected ErrVehicleNotFound, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id != tt.wantID {
				t.Errorf("vehicleID = %q, want %q", id, tt.wantID)
			}
		})
	}
}

func TestVINCache_CacheHitAvoidsDuplicateLookup(t *testing.T) {
	lookup := &stubVINLookup{
		vehicles: map[string]Vehicle{
			"5YJ3E1EA1NF000001": {ID: "veh_001", VIN: "5YJ3E1EA1NF000001"},
		},
	}
	cache := newVINCache(lookup, slog.Default())
	ctx := context.Background()

	// First call: cache miss → DB lookup.
	id1, err := cache.resolve(ctx, "5YJ3E1EA1NF000001")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Second call: cache hit → no DB lookup.
	id2, err := cache.resolve(ctx, "5YJ3E1EA1NF000001")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}

	if id1 != id2 {
		t.Errorf("ids differ: %q vs %q", id1, id2)
	}
	if calls := lookup.calls.Load(); calls != 1 {
		t.Errorf("DB lookups = %d, want 1 (cache should have hit)", calls)
	}
}

func TestVINCache_MissCachedPreventsRepeatedLookup(t *testing.T) {
	lookup := &stubVINLookup{vehicles: map[string]Vehicle{}}
	cache := newVINCache(lookup, slog.Default())
	ctx := context.Background()

	// First call: miss → DB lookup → ErrVehicleNotFound → cached.
	_, err := cache.resolve(ctx, "UNKNOWN")
	if !errors.Is(err, ErrVehicleNotFound) {
		t.Fatalf("expected ErrVehicleNotFound, got: %v", err)
	}

	// Second call: cached miss → no DB lookup.
	_, err = cache.resolve(ctx, "UNKNOWN")
	if !errors.Is(err, ErrVehicleNotFound) {
		t.Fatalf("expected ErrVehicleNotFound, got: %v", err)
	}

	if calls := lookup.calls.Load(); calls != 1 {
		t.Errorf("DB lookups = %d, want 1 (miss should be cached)", calls)
	}
}

func TestVINCache_TransientErrorNotCached(t *testing.T) {
	lookup := &stubVINLookup{
		vehicles: map[string]Vehicle{},
		err:      errors.New("connection refused"),
	}
	cache := newVINCache(lookup, slog.Default())
	ctx := context.Background()

	_, _ = cache.resolve(ctx, "5YJ3E1EA1NF000001")
	_, _ = cache.resolve(ctx, "5YJ3E1EA1NF000001")

	if calls := lookup.calls.Load(); calls != 2 {
		t.Errorf("DB lookups = %d, want 2 (transient errors should not be cached)", calls)
	}
}
