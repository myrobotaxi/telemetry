package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestVehicleCache_Lookup_CacheMiss(t *testing.T) {
	querier := &stubQuerier{ids: []string{"v1", "v2"}}
	cache := newVehicleCache(querier, 5*time.Minute)

	ids, err := cache.lookup(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 2 || ids[0] != "v1" || ids[1] != "v2" {
		t.Errorf("got %v, want [v1 v2]", ids)
	}
	if querier.callCount.Load() != 1 {
		t.Errorf("querier called %d times, want 1", querier.callCount.Load())
	}
}

func TestVehicleCache_Lookup_CacheHit(t *testing.T) {
	querier := &stubQuerier{ids: []string{"v1"}}
	cache := newVehicleCache(querier, 5*time.Minute)

	// First call populates cache.
	if _, err := cache.lookup(context.Background(), "user-1"); err != nil {
		t.Fatalf("first lookup: %v", err)
	}

	// Second call should hit cache.
	ids, err := cache.lookup(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if len(ids) != 1 || ids[0] != "v1" {
		t.Errorf("got %v, want [v1]", ids)
	}
	if querier.callCount.Load() != 1 {
		t.Errorf("querier called %d times, want 1 (cache should have been hit)", querier.callCount.Load())
	}
}

func TestVehicleCache_Lookup_Expiry(t *testing.T) {
	querier := &stubQuerier{ids: []string{"v1"}}
	cache := newVehicleCache(querier, 5*time.Minute)

	now := time.Now()
	cache.now = func() time.Time { return now }

	// Populate cache.
	if _, err := cache.lookup(context.Background(), "user-1"); err != nil {
		t.Fatalf("first lookup: %v", err)
	}

	// Advance time past TTL.
	cache.now = func() time.Time { return now.Add(6 * time.Minute) }

	// Should trigger a fresh query.
	if _, err := cache.lookup(context.Background(), "user-1"); err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if querier.callCount.Load() != 2 {
		t.Errorf("querier called %d times, want 2 (entry should have expired)", querier.callCount.Load())
	}
}

func TestVehicleCache_Lookup_QueryError(t *testing.T) {
	querier := &stubQuerier{err: errors.New("db down")}
	cache := newVehicleCache(querier, 5*time.Minute)

	_, err := cache.lookup(context.Background(), "user-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestVehicleCache_Lookup_EmptyResult(t *testing.T) {
	querier := &stubQuerier{ids: nil}
	cache := newVehicleCache(querier, 5*time.Minute)

	ids, err := cache.lookup(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ids != nil {
		t.Errorf("got %v, want nil (user has no vehicles)", ids)
	}
}
