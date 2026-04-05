package ws

import (
	"testing"
	"time"
)

func TestRouteAccumulator_Add_BatchSizeTrigger(t *testing.T) {
	acc := newRouteAccumulator(3, 0) // batch size 3, no interval

	vin := "5YJ3E1EA1NF000001"
	coords := []routeCoordinate{
		{Latitude: 37.7749, Longitude: -122.4194},
		{Latitude: 37.7750, Longitude: -122.4195},
		{Latitude: 37.7751, Longitude: -122.4196},
	}

	// First two should not trigger flush.
	for i := 0; i < 2; i++ {
		result := acc.Add(vin, coords[i])
		if result.ShouldFlush {
			t.Fatalf("point %d: expected ShouldFlush=false", i)
		}
		if result.Points != nil {
			t.Fatalf("point %d: expected nil Points", i)
		}
	}

	// Third should trigger flush with all 3 points.
	result := acc.Add(vin, coords[2])
	if !result.ShouldFlush {
		t.Fatal("expected ShouldFlush=true after batch size reached")
	}
	if len(result.Points) != 3 {
		t.Fatalf("expected 3 points, got %d", len(result.Points))
	}

	// Verify points are in order.
	for i, p := range result.Points {
		if p.Latitude != coords[i].Latitude || p.Longitude != coords[i].Longitude {
			t.Fatalf("point %d mismatch: got (%f, %f), want (%f, %f)",
				i, p.Latitude, p.Longitude, coords[i].Latitude, coords[i].Longitude)
		}
	}
}

func TestRouteAccumulator_Add_IntervalTrigger(t *testing.T) {
	// Large batch size so only interval triggers.
	acc := newRouteAccumulator(100, 2*time.Second)

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	acc.now = func() time.Time { return now }

	vin := "5YJ3E1EA1NF000001"

	// First point: initializes timer, no flush.
	result := acc.Add(vin, routeCoordinate{Latitude: 37.0, Longitude: -122.0})
	if result.ShouldFlush {
		t.Fatal("expected no flush on first point")
	}

	// Second point 1 second later: still within interval.
	now = now.Add(1 * time.Second)
	result = acc.Add(vin, routeCoordinate{Latitude: 37.1, Longitude: -122.1})
	if result.ShouldFlush {
		t.Fatal("expected no flush within interval")
	}

	// Third point 2 seconds later (total 3s from first): should trigger.
	now = now.Add(2 * time.Second)
	result = acc.Add(vin, routeCoordinate{Latitude: 37.2, Longitude: -122.2})
	if !result.ShouldFlush {
		t.Fatal("expected flush after interval elapsed")
	}
	if len(result.Points) != 3 {
		t.Fatalf("expected 3 points, got %d", len(result.Points))
	}
}

func TestRouteAccumulator_Add_MultipleVINs(t *testing.T) {
	acc := newRouteAccumulator(2, 0)

	vin1 := "5YJ3E1EA1NF000001"
	vin2 := "5YJ3E1EA1NF000002"

	// Add 1 point to each VIN — neither should flush.
	r1 := acc.Add(vin1, routeCoordinate{Latitude: 37.0, Longitude: -122.0})
	r2 := acc.Add(vin2, routeCoordinate{Latitude: 40.0, Longitude: -74.0})
	if r1.ShouldFlush || r2.ShouldFlush {
		t.Fatal("expected no flush with 1 point each")
	}

	// Add second point to vin1 — only vin1 should flush.
	r1 = acc.Add(vin1, routeCoordinate{Latitude: 37.1, Longitude: -122.1})
	if !r1.ShouldFlush {
		t.Fatal("expected vin1 to flush")
	}
	if len(r1.Points) != 2 {
		t.Fatalf("expected 2 points for vin1, got %d", len(r1.Points))
	}

	// vin2 still has 1 point — adding second should flush.
	r2 = acc.Add(vin2, routeCoordinate{Latitude: 40.1, Longitude: -74.1})
	if !r2.ShouldFlush {
		t.Fatal("expected vin2 to flush")
	}
	if len(r2.Points) != 2 {
		t.Fatalf("expected 2 points for vin2, got %d", len(r2.Points))
	}
}

func TestRouteAccumulator_Flush(t *testing.T) {
	acc := newRouteAccumulator(100, 0) // large batch, no interval

	vin := "5YJ3E1EA1NF000001"
	acc.Add(vin, routeCoordinate{Latitude: 37.0, Longitude: -122.0})
	acc.Add(vin, routeCoordinate{Latitude: 37.1, Longitude: -122.1})

	points := acc.Flush(vin)
	if len(points) != 2 {
		t.Fatalf("expected 2 points, got %d", len(points))
	}

	// Second flush returns same points (buffer persists until Clear).
	points = acc.Flush(vin)
	if len(points) != 2 {
		t.Fatalf("expected 2 points on second flush, got %d", len(points))
	}

	// After Clear, flush returns nil.
	acc.Clear(vin)
	points = acc.Flush(vin)
	if points != nil {
		t.Fatal("expected nil after clear")
	}
}

func TestRouteAccumulator_Flush_EmptyVIN(t *testing.T) {
	acc := newRouteAccumulator(5, 0)

	points := acc.Flush("nonexistent")
	if points != nil {
		t.Fatal("expected nil for unknown VIN")
	}
}

func TestRouteAccumulator_Clear(t *testing.T) {
	acc := newRouteAccumulator(100, 0)

	vin := "5YJ3E1EA1NF000001"
	acc.Add(vin, routeCoordinate{Latitude: 37.0, Longitude: -122.0})
	acc.Add(vin, routeCoordinate{Latitude: 37.1, Longitude: -122.1})

	acc.Clear(vin)

	// Flush should return nil after clear.
	points := acc.Flush(vin)
	if points != nil {
		t.Fatal("expected nil after clear")
	}
}

func TestRouteAccumulator_Clear_UnknownVIN(t *testing.T) {
	acc := newRouteAccumulator(5, 0)

	// Should not panic.
	acc.Clear("nonexistent")
}

func TestCoordsToMapbox(t *testing.T) {
	tests := []struct {
		name   string
		points []routeCoordinate
		want   [][]float64
	}{
		{
			name:   "empty",
			points: []routeCoordinate{},
			want:   [][]float64{},
		},
		{
			name: "single point",
			points: []routeCoordinate{
				{Latitude: 37.7749, Longitude: -122.4194},
			},
			want: [][]float64{{-122.4194, 37.7749}},
		},
		{
			name: "multiple points in lng/lat order",
			points: []routeCoordinate{
				{Latitude: 37.7749, Longitude: -122.4194},
				{Latitude: 40.7128, Longitude: -74.0060},
			},
			want: [][]float64{
				{-122.4194, 37.7749},
				{-74.0060, 40.7128},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coordsToMapbox(tt.points)
			if len(got) != len(tt.want) {
				t.Fatalf("expected %d coords, got %d", len(tt.want), len(got))
			}
			for i := range got {
				if got[i][0] != tt.want[i][0] || got[i][1] != tt.want[i][1] {
					t.Fatalf("coord %d: got [%f, %f], want [%f, %f]",
						i, got[i][0], got[i][1], tt.want[i][0], tt.want[i][1])
				}
			}
		})
	}
}

func TestRouteAccumulator_FullTrailAfterFlush(t *testing.T) {
	acc := newRouteAccumulator(2, 0)

	vin := "5YJ3E1EA1NF000001"

	// Fill and flush first batch (2 points).
	acc.Add(vin, routeCoordinate{Latitude: 1.0, Longitude: 2.0})
	result := acc.Add(vin, routeCoordinate{Latitude: 3.0, Longitude: 4.0})
	if !result.ShouldFlush {
		t.Fatal("expected flush")
	}
	if len(result.Points) != 2 {
		t.Fatalf("expected 2 points in first flush, got %d", len(result.Points))
	}

	// Add 2 more points — second flush contains ALL 4 points (full trail).
	acc.Add(vin, routeCoordinate{Latitude: 5.0, Longitude: 6.0})
	result = acc.Add(vin, routeCoordinate{Latitude: 7.0, Longitude: 8.0})
	if !result.ShouldFlush {
		t.Fatal("expected flush on second batch")
	}
	if len(result.Points) != 4 {
		t.Fatalf("expected 4 points (full trail), got %d", len(result.Points))
	}
	// Verify all points are present in order.
	if result.Points[0].Latitude != 1.0 {
		t.Fatalf("expected first point lat=1.0, got %f", result.Points[0].Latitude)
	}
	if result.Points[3].Latitude != 7.0 {
		t.Fatalf("expected fourth point lat=7.0, got %f", result.Points[3].Latitude)
	}
}
