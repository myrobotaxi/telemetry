package simulator

import (
	"testing"
)

func TestNewScenario(t *testing.T) {
	tests := []struct {
		name     string
		wantName string
		wantNil  bool
	}{
		{name: "highway-drive", wantName: "highway-drive"},
		{name: "city-drive", wantName: "city-drive"},
		{name: "parking-lot", wantName: "parking-lot"},
		{name: "unknown", wantNil: true},
		{name: "", wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewScenario(tt.name)
			if tt.wantNil {
				if s != nil {
					t.Fatalf("NewScenario(%q) = %v, want nil", tt.name, s)
				}
				return
			}
			if s == nil {
				t.Fatalf("NewScenario(%q) = nil, want non-nil", tt.name)
			}
			if got := s.Name(); got != tt.wantName {
				t.Errorf("Name() = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestHighwayDrive_StartsParked(t *testing.T) {
	s := newHighwayDrive(scenarioConfig{})
	state := s.Next()

	if state.GearPosition != "P" {
		t.Errorf("initial gear = %q, want P", state.GearPosition)
	}
	if state.Speed != 0 {
		t.Errorf("initial speed = %f, want 0", state.Speed)
	}
}

func TestHighwayDrive_EndsParked(t *testing.T) {
	s := newHighwayDrive(scenarioConfig{})

	var lastState ScenarioState
	for !s.Done() {
		lastState = s.Next()
	}

	if lastState.GearPosition != "P" {
		t.Errorf("final gear = %q, want P", lastState.GearPosition)
	}
	if lastState.Speed != 0 {
		t.Errorf("final speed = %f, want 0", lastState.Speed)
	}
}

func TestHighwayDrive_ReachesHighwaySpeed(t *testing.T) {
	s := newHighwayDrive(scenarioConfig{})

	var maxSpeed float64
	for !s.Done() {
		state := s.Next()
		if state.Speed > maxSpeed {
			maxSpeed = state.Speed
		}
	}

	if maxSpeed < 60 {
		t.Errorf("max speed = %f, want >= 60 mph", maxSpeed)
	}
}

func TestHighwayDrive_DrainsCharge(t *testing.T) {
	s := newHighwayDrive(scenarioConfig{})

	first := s.Next()
	var last ScenarioState
	for !s.Done() {
		last = s.Next()
	}

	if last.ChargeLevel >= first.ChargeLevel {
		t.Errorf("charge did not drain: start=%d, end=%d", first.ChargeLevel, last.ChargeLevel)
	}
}

func TestHighwayDrive_AdvancesPosition(t *testing.T) {
	s := newHighwayDrive(scenarioConfig{})

	first := s.Next()
	var last ScenarioState
	for !s.Done() {
		last = s.Next()
	}

	if last.Latitude == first.Latitude && last.Longitude == first.Longitude {
		t.Error("position did not change during drive")
	}
	if last.OdometerMiles <= first.OdometerMiles {
		t.Errorf("odometer did not advance: start=%f, end=%f",
			first.OdometerMiles, last.OdometerMiles)
	}
}

func TestCityDrive_HasStopAndGo(t *testing.T) {
	s := newCityDrive(scenarioConfig{})

	var transitions int
	var wasMoving bool
	for !s.Done() {
		state := s.Next()
		isMoving := state.Speed > 0
		if isMoving != wasMoving {
			transitions++
		}
		wasMoving = isMoving
	}

	// City drive should have multiple stop/go transitions.
	if transitions < 4 {
		t.Errorf("transitions = %d, want >= 4 for stop-and-go pattern", transitions)
	}
}

func TestParkingLot_HasRapidGearChanges(t *testing.T) {
	s := newParkingLot()

	gearChanges := 0
	prevGear := ""
	seenGears := make(map[string]bool)

	for !s.Done() {
		state := s.Next()
		seenGears[state.GearPosition] = true
		if prevGear != "" && state.GearPosition != prevGear {
			gearChanges++
		}
		prevGear = state.GearPosition
	}

	if gearChanges < 5 {
		t.Errorf("gear changes = %d, want >= 5 for parking-lot scenario", gearChanges)
	}
	if !seenGears["P"] || !seenGears["D"] || !seenGears["R"] {
		t.Errorf("expected P, D, R gears; got %v", seenGears)
	}
}

func TestEtaMinutes(t *testing.T) {
	tests := []struct {
		name        string
		tick        int
		total       int
		intervalSec float64
		want        float64
	}{
		{name: "start of 1800-tick scenario", tick: 0, total: 1800, intervalSec: 1.0, want: 30.0},
		{name: "halfway through", tick: 900, total: 1800, intervalSec: 1.0, want: 15.0},
		{name: "at the end", tick: 1800, total: 1800, intervalSec: 1.0, want: 0},
		{name: "past the end", tick: 2000, total: 1800, intervalSec: 1.0, want: 0},
		{name: "2s interval", tick: 0, total: 900, intervalSec: 2.0, want: 30.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := etaMinutes(tt.tick, tt.total, tt.intervalSec)
			if got != tt.want {
				t.Errorf("etaMinutes(%d, %d, %f) = %f, want %f",
					tt.tick, tt.total, tt.intervalSec, got, tt.want)
			}
		})
	}
}

func TestHighwayDrive_ETACountdown(t *testing.T) {
	s := newHighwayDrive(scenarioConfig{})

	first := s.Next()
	if first.ETA <= 0 {
		t.Fatalf("first tick ETA = %f, want > 0", first.ETA)
	}

	// Highway drive is 1800 ticks at 1s interval = 30 minutes.
	if first.ETA < 29 || first.ETA > 30 {
		t.Errorf("first ETA = %f, want ~30 minutes", first.ETA)
	}

	var last ScenarioState
	for !s.Done() {
		last = s.Next()
	}

	if first.ETA <= last.ETA {
		t.Errorf("ETA did not decrease: first=%f, last=%f", first.ETA, last.ETA)
	}
	// Last tick computes ETA for remaining=1, so it is close to but not exactly 0.
	if last.ETA > 0.1 {
		t.Errorf("final ETA = %f, want < 0.1 (nearly done)", last.ETA)
	}
}

func TestCityDrive_ETACountdown(t *testing.T) {
	s := newCityDrive(scenarioConfig{})

	first := s.Next()
	if first.ETA <= 0 {
		t.Fatalf("first tick ETA = %f, want > 0", first.ETA)
	}

	var last ScenarioState
	for !s.Done() {
		last = s.Next()
	}

	if last.ETA > 0.1 {
		t.Errorf("final ETA = %f, want < 0.1 (nearly done)", last.ETA)
	}
}

func TestParkingLot_ETACountdown(t *testing.T) {
	s := newParkingLot()

	first := s.Next()
	if first.ETA <= 0 {
		t.Fatalf("first tick ETA = %f, want > 0", first.ETA)
	}

	var last ScenarioState
	for !s.Done() {
		last = s.Next()
	}

	if last.ETA > 0.1 {
		t.Errorf("final ETA = %f, want < 0.1 (nearly done)", last.ETA)
	}
}

func TestHighwayDrive_WithRoute_SetsNavFields(t *testing.T) {
	rf := &RouteFile{
		Name:               "Test Highway",
		Origin:             RoutePoint{Name: "Start", Lat: 32.7767, Lng: -96.7970},
		Destination:        RoutePoint{Name: "Dest", Lat: 33.1972, Lng: -96.6153},
		TotalDistanceMiles: 34.5,
		Coordinates: [][2]float64{
			{-96.7970, 32.7767},
			{-96.7000, 32.9000},
			{-96.6153, 33.1972},
		},
	}
	s := newHighwayDrive(scenarioConfig{routeFile: rf})

	first := s.Next()
	if first.Nav.TripDistanceMiles != 34.5 {
		t.Errorf("TripDistanceMiles = %f, want 34.5", first.Nav.TripDistanceMiles)
	}
	if first.Nav.DestinationName != "Dest" {
		t.Errorf("DestinationName = %q, want %q", first.Nav.DestinationName, "Dest")
	}
	if first.Nav.DestinationLat != 33.1972 {
		t.Errorf("DestinationLat = %f, want 33.1972", first.Nav.DestinationLat)
	}
	if first.Nav.RouteLine == "" {
		t.Error("RouteLine should not be empty")
	}
}

func TestHighwayDrive_WithRoute_FollowsRoad(t *testing.T) {
	rf := &RouteFile{
		Name:               "Test Highway",
		Origin:             RoutePoint{Name: "Start", Lat: 32.0, Lng: -96.0},
		Destination:        RoutePoint{Name: "End", Lat: 33.0, Lng: -96.0},
		TotalDistanceMiles: 69.0,
		Coordinates: [][2]float64{
			{-96.0, 32.0},
			{-96.0, 32.5},
			{-96.0, 33.0},
		},
	}
	s := newHighwayDrive(scenarioConfig{routeFile: rf})

	// Run through ~100 ticks to get past park phase and into driving.
	var latitudes []float64
	for i := 0; i < 100 && !s.Done(); i++ {
		state := s.Next()
		latitudes = append(latitudes, state.Latitude)
	}

	// After accelerating (tick 5-30), latitude should increase along the route.
	// The vehicle starts at 32.0 and should move northward.
	lastLat := latitudes[len(latitudes)-1]
	if lastLat <= 32.0 {
		t.Errorf("highway drive with north-bound route should move north, got lat=%f", lastLat)
	}
}

func TestHighwayDrive_WithoutRoute_RandomWalk(t *testing.T) {
	s := newHighwayDrive(scenarioConfig{})

	first := s.Next()
	if first.Nav.RouteLine != "" {
		t.Error("RouteLine should be empty without route file")
	}
	if first.Nav.TripDistanceMiles != 0 {
		t.Errorf("TripDistanceMiles = %f, want 0", first.Nav.TripDistanceMiles)
	}
}

func TestScenarioNames(t *testing.T) {
	names := ScenarioNames()
	if len(names) != 3 {
		t.Fatalf("ScenarioNames() returned %d names, want 3", len(names))
	}

	// Every name should produce a valid scenario.
	for _, name := range names {
		if s := NewScenario(name); s == nil {
			t.Errorf("NewScenario(%q) = nil, want valid scenario", name)
		}
	}
}
