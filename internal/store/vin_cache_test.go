package store

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
)

// stubIDLookup is a test double for vinIDLookup that returns configurable
// (id, userID) pairs and counts calls.
type stubIDLookup struct {
	pairs map[string]struct{ id, userID string }
	err   error
	calls atomic.Int64
}

func (s *stubIDLookup) GetIDsByVIN(_ context.Context, vin string) (string, string, error) {
	s.calls.Add(1)
	if s.err != nil {
		return "", "", s.err
	}
	p, ok := s.pairs[vin]
	if !ok {
		return "", "", ErrVehicleNotFound
	}
	return p.id, p.userID, nil
}

func TestVINCache_ResolveID(t *testing.T) {
	logger := slog.Default()

	tests := []struct {
		name      string
		pairs     map[string]struct{ id, userID string }
		lookupErr error
		vin       string
		wantID    string
		wantErr   error
	}{
		{
			name: "cache miss then hit",
			pairs: map[string]struct{ id, userID string }{
				"5YJ3E1EA1NF000001": {id: "veh_001", userID: "user_001"},
			},
			vin:    "5YJ3E1EA1NF000001",
			wantID: "veh_001",
		},
		{
			name:    "vehicle not found cached",
			pairs:   map[string]struct{ id, userID string }{},
			vin:     "UNKNOWN",
			wantErr: ErrVehicleNotFound,
		},
		{
			name:      "transient error not cached",
			pairs:     map[string]struct{ id, userID string }{},
			lookupErr: errors.New("connection refused"),
			vin:       "5YJ3E1EA1NF000001",
			wantErr:   errors.New("connection refused"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := &stubIDLookup{pairs: tt.pairs, err: tt.lookupErr}
			cache := NewVINCache(lookup, logger)
			ctx := context.Background()

			id, err := cache.ResolveID(ctx, tt.vin)
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

func TestVINCache_ResolveOwnerReusesSameEntry(t *testing.T) {
	lookup := &stubIDLookup{
		pairs: map[string]struct{ id, userID string }{
			"5YJ3E1EA1NF000001": {id: "veh_001", userID: "user_owner"},
		},
	}
	cache := NewVINCache(lookup, slog.Default())
	ctx := context.Background()

	// Resolve the ID first (one DB lookup).
	id, err := cache.ResolveID(ctx, "5YJ3E1EA1NF000001")
	if err != nil {
		t.Fatalf("ResolveID: %v", err)
	}
	if id != "veh_001" {
		t.Errorf("id = %q, want veh_001", id)
	}

	// Then resolve the owner — should hit the same cached entry, no new DB call.
	owner, err := cache.ResolveOwner(ctx, "5YJ3E1EA1NF000001")
	if err != nil {
		t.Fatalf("ResolveOwner: %v", err)
	}
	if owner != "user_owner" {
		t.Errorf("owner = %q, want user_owner", owner)
	}

	if calls := lookup.calls.Load(); calls != 1 {
		t.Errorf("DB lookups = %d, want 1 (single cache entry serves both methods)", calls)
	}
}

func TestVINCache_CacheHitAvoidsDuplicateLookup(t *testing.T) {
	lookup := &stubIDLookup{
		pairs: map[string]struct{ id, userID string }{
			"5YJ3E1EA1NF000001": {id: "veh_001", userID: "user_001"},
		},
	}
	cache := NewVINCache(lookup, slog.Default())
	ctx := context.Background()

	id1, err := cache.ResolveID(ctx, "5YJ3E1EA1NF000001")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	id2, err := cache.ResolveID(ctx, "5YJ3E1EA1NF000001")
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
	lookup := &stubIDLookup{pairs: map[string]struct{ id, userID string }{}}
	cache := NewVINCache(lookup, slog.Default())
	ctx := context.Background()

	_, err := cache.ResolveID(ctx, "UNKNOWN")
	if !errors.Is(err, ErrVehicleNotFound) {
		t.Fatalf("expected ErrVehicleNotFound, got: %v", err)
	}

	// Owner lookup should also see the cached miss without a DB hit.
	_, err = cache.ResolveOwner(ctx, "UNKNOWN")
	if !errors.Is(err, ErrVehicleNotFound) {
		t.Fatalf("expected ErrVehicleNotFound, got: %v", err)
	}

	if calls := lookup.calls.Load(); calls != 1 {
		t.Errorf("DB lookups = %d, want 1 (miss should be cached and shared across methods)", calls)
	}
}

func TestVINCache_TransientErrorNotCached(t *testing.T) {
	lookup := &stubIDLookup{
		pairs: map[string]struct{ id, userID string }{},
		err:   errors.New("connection refused"),
	}
	cache := NewVINCache(lookup, slog.Default())
	ctx := context.Background()

	_, _ = cache.ResolveID(ctx, "5YJ3E1EA1NF000001")
	_, _ = cache.ResolveID(ctx, "5YJ3E1EA1NF000001")

	if calls := lookup.calls.Load(); calls != 2 {
		t.Errorf("DB lookups = %d, want 2 (transient errors should not be cached)", calls)
	}
}
