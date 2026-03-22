package simulator

import (
	"math"
	"testing"
)

func TestCityDrive_WithRoute_FollowsRoad(t *testing.T) {
	rf := &RouteFile{
		Name:               "City Test",
		Origin:             RoutePoint{Name: "Start", Lat: 32.7767, Lng: -96.7970},
		Destination:        RoutePoint{Name: "End", Lat: 32.8020, Lng: -96.8010},
		TotalDistanceMiles: 5.0,
		Coordinates: [][2]float64{
			{-96.7970, 32.7767},
			{-96.7960, 32.7780},
			{-96.7950, 32.7800},
			{-96.8010, 32.8020},
		},
	}
	cfg := scenarioConfig{routeFile: rf}
	s := newCityDrive(cfg)

	// Verify nav fields are populated.
	first := s.Next()
	if first.Nav.DestinationName != "End" {
		t.Errorf("DestinationName = %q, want %q", first.Nav.DestinationName, "End")
	}
	if first.Nav.TripDistanceMiles != 5.0 {
		t.Errorf("TripDistanceMiles = %f, want 5.0", first.Nav.TripDistanceMiles)
	}
	if first.Nav.RouteLine == "" {
		t.Error("RouteLine should not be empty with route file")
	}
}

func TestCityDrive_WithoutRoute_RandomWalk(t *testing.T) {
	s := newCityDrive(scenarioConfig{})

	first := s.Next()
	if first.Nav.RouteLine != "" {
		t.Error("RouteLine should be empty without route file")
	}
	if first.Nav.DestinationName != "" {
		t.Errorf("DestinationName = %q, want empty", first.Nav.DestinationName)
	}
}

func TestCityDrive_WithRoute_PositionChanges(t *testing.T) {
	rf := &RouteFile{
		Name:               "Test",
		Origin:             RoutePoint{Name: "A", Lat: 32.0, Lng: -96.0},
		Destination:        RoutePoint{Name: "B", Lat: 33.0, Lng: -96.0},
		TotalDistanceMiles: 69.0,
		Coordinates: [][2]float64{
			{-96.0, 32.0},
			{-96.0, 32.5},
			{-96.0, 33.0},
		},
	}
	s := newCityDrive(scenarioConfig{routeFile: rf})

	// Run enough ticks to get past the initial parked phase (5 ticks)
	// and through some movement.
	var positions []float64
	for i := 0; i < 100 && !s.Done(); i++ {
		state := s.Next()
		positions = append(positions, state.Latitude)
	}

	// At least some ticks should have moved from start.
	moved := false
	for _, lat := range positions {
		if math.Abs(lat-32.0) > 0.001 {
			moved = true
			break
		}
	}
	if !moved {
		t.Error("city drive with route should have moved from starting position")
	}
}
