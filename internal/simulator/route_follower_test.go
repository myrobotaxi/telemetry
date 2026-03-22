package simulator

import (
	"math"
	"testing"
)

// testRoute returns a simple north-south route for testing.
func testRoute() *RouteFile {
	return &RouteFile{
		Name:               "Test North-South",
		Origin:             RoutePoint{Name: "South", Lat: 32.0, Lng: -96.0},
		Destination:        RoutePoint{Name: "North", Lat: 33.0, Lng: -96.0},
		TotalDistanceMiles: 69.0, // ~1 degree of latitude
		Coordinates: [][2]float64{
			{-96.0, 32.0},
			{-96.0, 32.5},
			{-96.0, 33.0},
		},
	}
}

func TestRouteFollower_InitialPosition(t *testing.T) {
	f := NewRouteFollower(testRoute())
	pos := f.Position()

	if math.Abs(pos.Lat-32.0) > 0.001 {
		t.Errorf("initial lat = %f, want ~32.0", pos.Lat)
	}
	if math.Abs(pos.Lng-(-96.0)) > 0.001 {
		t.Errorf("initial lng = %f, want ~-96.0", pos.Lng)
	}
	if pos.Finished {
		t.Error("initial position should not be finished")
	}
}

func TestRouteFollower_AdvancesNorth(t *testing.T) {
	f := NewRouteFollower(testRoute())

	// Advance at 60 mph for 60 seconds = 1 mile northward.
	pos := f.Advance(60, 60)

	if pos.Lat <= 32.0 {
		t.Errorf("lat should increase heading north, got %f", pos.Lat)
	}
	if math.Abs(pos.Lng-(-96.0)) > 0.01 {
		t.Errorf("lng should stay near -96.0, got %f", pos.Lng)
	}
	// Heading should be ~0 (north) or ~360.
	if pos.Heading > 1 && pos.Heading < 359 {
		t.Errorf("heading should be ~0 (north), got %f", pos.Heading)
	}
}

func TestRouteFollower_DistanceDecreases(t *testing.T) {
	f := NewRouteFollower(testRoute())

	initial := f.Position()
	_ = f.Advance(60, 60)
	after := f.Position()

	if after.DistanceRemain >= initial.DistanceRemain {
		t.Errorf("distance should decrease: initial=%f, after=%f",
			initial.DistanceRemain, after.DistanceRemain)
	}
}

func TestRouteFollower_FinishesAtEnd(t *testing.T) {
	f := NewRouteFollower(testRoute())

	// Advance far enough to finish the route (69 miles).
	// 60 mph * 3600 seconds = 60 miles per iteration.
	for i := 0; i < 10; i++ {
		f.Advance(60, 3600)
	}

	pos := f.Position()
	if !pos.Finished {
		t.Error("expected route to be finished")
	}
	if pos.DistanceRemain != 0 {
		t.Errorf("distance remain = %f, want 0", pos.DistanceRemain)
	}
	// Should be at destination.
	if math.Abs(pos.Lat-33.0) > 0.01 {
		t.Errorf("final lat = %f, want ~33.0", pos.Lat)
	}
}

func TestRouteFollower_StationaryNoMovement(t *testing.T) {
	f := NewRouteFollower(testRoute())
	pos := f.Advance(0, 60)

	if math.Abs(pos.Lat-32.0) > 0.001 {
		t.Errorf("lat changed despite zero speed: %f", pos.Lat)
	}
}

func TestRouteFollower_InterpolatesBetweenWaypoints(t *testing.T) {
	rf := &RouteFile{
		Name:               "Short",
		Origin:             RoutePoint{Name: "A", Lat: 32.0, Lng: -96.0},
		Destination:        RoutePoint{Name: "B", Lat: 32.0, Lng: -95.0},
		TotalDistanceMiles: 60.0,
		Coordinates: [][2]float64{
			{-96.0, 32.0},
			{-95.0, 32.0},
		},
	}

	f := NewRouteFollower(rf)
	// Move a small distance — should be between waypoints.
	pos := f.Advance(30, 60) // 0.5 miles

	if pos.Lat < 31.99 || pos.Lat > 32.01 {
		t.Errorf("lat should be near 32.0 (interpolated), got %f", pos.Lat)
	}
	if pos.Lng <= -96.0 || pos.Lng >= -95.0 {
		t.Errorf("lng should be between -96.0 and -95.0 (interpolated), got %f", pos.Lng)
	}
}

func TestBearing_Cardinal(t *testing.T) {
	tests := []struct {
		name     string
		lat1     float64
		lng1     float64
		lat2     float64
		lng2     float64
		wantNear float64
		tolerance float64
	}{
		{"north", 32.0, -96.0, 33.0, -96.0, 0, 1},
		{"south", 33.0, -96.0, 32.0, -96.0, 180, 1},
		{"east", 32.0, -96.0, 32.0, -95.0, 90, 1},
		{"west", 32.0, -95.0, 32.0, -96.0, 270, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bearing(tt.lat1, tt.lng1, tt.lat2, tt.lng2)
			diff := math.Abs(got - tt.wantNear)
			if diff > 180 {
				diff = 360 - diff
			}
			if diff > tt.tolerance {
				t.Errorf("bearing = %f, want ~%f (diff=%f)", got, tt.wantNear, diff)
			}
		})
	}
}

func TestHaversineDistance(t *testing.T) {
	// 1 degree of latitude is ~69 miles.
	d := haversineDistance(32.0, -96.0, 33.0, -96.0)
	if math.Abs(d-69.0) > 1.0 {
		t.Errorf("haversineDistance for 1 deg lat = %f, want ~69 miles", d)
	}
}

func TestComputeSegmentLengths(t *testing.T) {
	coords := [][2]float64{
		{-96.0, 32.0},
		{-96.0, 32.5},
		{-96.0, 33.0},
	}

	lengths := computeSegmentLengths(coords)
	if len(lengths) != 2 {
		t.Fatalf("segments = %d, want 2", len(lengths))
	}
	// Each segment is 0.5 degrees lat ~ 34.5 miles.
	for i, l := range lengths {
		if l < 33 || l > 36 {
			t.Errorf("segment[%d] length = %f, want ~34.5 miles", i, l)
		}
	}
}

func TestComputeSegmentLengths_TooFewCoords(t *testing.T) {
	lengths := computeSegmentLengths([][2]float64{{-96.0, 32.0}})
	if lengths != nil {
		t.Errorf("expected nil for single coordinate, got %v", lengths)
	}
}
