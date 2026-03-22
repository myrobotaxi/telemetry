package simulator

import (
	"log/slog"

	"math/rand/v2" //nolint:gosec // simulation data does not require crypto/rand
)

// Starting point for all scenarios: downtown Dallas, TX.
const (
	startLat = 32.7767
	startLng = -96.7970
)

// scenarioDefaults are shared initial values for all scenarios.
func scenarioDefaults() ScenarioState {
	return ScenarioState{
		Speed:          0,
		Latitude:       startLat,
		Longitude:      startLng,
		Heading:        45, // northeast
		GearPosition:   "P",
		ChargeLevel:    88,
		EstimatedRange: 220,
		InteriorTemp:   22,
		ExteriorTemp:   28,
		OdometerMiles:  12450.3,
	}
}

// NewScenario creates a named scenario. Returns nil if the name is unknown.
// Supported names: highway-drive, city-drive, parking-lot.
//
// For highway-drive and city-drive, pass WithRouteFile to drive along
// pre-baked road coordinates. Without a route file, the scenario falls
// back to random-walk positioning.
func NewScenario(name string, opts ...ScenarioOption) Scenario {
	cfg := scenarioConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	switch name {
	case "highway-drive":
		return newHighwayDrive(cfg)
	case "city-drive":
		return newCityDrive(cfg)
	case "parking-lot":
		return newParkingLot()
	default:
		return nil
	}
}

// ScenarioOption configures scenario construction.
type ScenarioOption func(*scenarioConfig)

type scenarioConfig struct {
	routeFile *RouteFile
	logger    *slog.Logger
}

// WithRouteFile provides a pre-loaded route file for the scenario.
func WithRouteFile(rf *RouteFile) ScenarioOption {
	return func(c *scenarioConfig) { c.routeFile = rf }
}

// WithLogger provides a logger for the scenario.
func WithLogger(l *slog.Logger) ScenarioOption {
	return func(c *scenarioConfig) { c.logger = l }
}

// ScenarioNames returns the list of supported scenario names.
func ScenarioNames() []string {
	return []string{"highway-drive", "city-drive", "parking-lot"}
}

// --- Highway Drive ---

// highwayDrive simulates a ~30-minute highway trip: park -> accelerate -> cruise
// at 65-75 mph -> decelerate -> park. When a route file is loaded, the vehicle
// follows pre-baked road coordinates instead of random-walking.
type highwayDrive struct {
	state    ScenarioState
	tick     int
	total    int // total ticks (1 tick = 1 second at default interval)
	follower *RouteFollower
}

func newHighwayDrive(cfg scenarioConfig) *highwayDrive {
	h := &highwayDrive{
		state: scenarioDefaults(),
		total: 1800, // 30 minutes
	}
	if cfg.routeFile != nil {
		h.follower = NewRouteFollower(cfg.routeFile)
		h.state.Nav.TripDistanceMiles = cfg.routeFile.TotalDistanceMiles
		h.state.Nav.TripDistanceRemain = cfg.routeFile.TotalDistanceMiles
		h.state.Nav.RouteLine = EncodePolyline(cfg.routeFile.Coordinates)
		h.state.Nav.DestinationName = cfg.routeFile.Destination.Name
		h.state.Nav.DestinationLat = cfg.routeFile.Destination.Lat
		h.state.Nav.DestinationLng = cfg.routeFile.Destination.Lng
	}
	return h
}

func (h *highwayDrive) Name() string { return "highway-drive" }
func (h *highwayDrive) Done() bool   { return h.tick >= h.total }

func (h *highwayDrive) Next() ScenarioState {
	defer func() { h.tick++ }()

	h.updateGear()
	h.updateSpeed()
	h.updatePosition()
	h.drainCharge()
	h.state.ETA = etaMinutes(h.tick, h.total, h.interval())

	return h.state
}

func (h *highwayDrive) interval() float64 { return 1.0 }

func (h *highwayDrive) updateGear() {
	switch {
	case h.tick < 5:
		h.state.GearPosition = "P"
	case h.tick >= h.total-5:
		h.state.GearPosition = "P"
		h.state.Speed = 0
	default:
		h.state.GearPosition = "D"
	}
}

func (h *highwayDrive) updateSpeed() {
	if h.state.GearPosition != "D" {
		return
	}
	switch {
	case h.tick < 30: // accelerate over ~25 seconds
		h.state.Speed = clampSpeed(h.state.Speed+2.5, 0, 75)
	case h.tick >= h.total-30: // decelerate
		h.state.Speed = clampSpeed(h.state.Speed-2.5, 0, 75)
	default: // cruise with small random variation
		h.state.Speed = clampSpeed(70+jitter(10), 60, 80)
	}
}

func (h *highwayDrive) updatePosition() {
	if h.follower != nil {
		h.updatePositionFromRoute()
		return
	}
	h.updatePositionRandomWalk()
}

func (h *highwayDrive) updatePositionFromRoute() {
	pos := h.follower.Advance(h.state.Speed, 1.0)
	h.state.Latitude = pos.Lat
	h.state.Longitude = pos.Lng
	h.state.Heading = pos.Heading
	h.state.Nav.TripDistanceRemain = pos.DistanceRemain
	if h.state.Speed > 0 {
		h.state.OdometerMiles += h.state.Speed / 3600.0
	}
}

func (h *highwayDrive) updatePositionRandomWalk() {
	h.state.Heading = normalizeHeading(h.state.Heading + jitter(2))
	h.state.Latitude, h.state.Longitude = advancePosition(
		h.state.Latitude, h.state.Longitude,
		h.state.Heading, h.state.Speed, 1.0,
	)
	if h.state.Speed > 0 {
		h.state.OdometerMiles += h.state.Speed / 3600.0
	}
}

func (h *highwayDrive) drainCharge() {
	// Lose ~1% charge every 90 seconds while driving.
	if h.tick > 0 && h.tick%90 == 0 && h.state.Speed > 0 {
		h.state.ChargeLevel = max(h.state.ChargeLevel-1, 10)
		h.state.EstimatedRange = max(h.state.EstimatedRange-3, 25)
	}
}

// clampSpeed constrains v to [lo, hi].
func clampSpeed(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// etaMinutes computes the estimated time of arrival in minutes based on
// remaining ticks and the interval in seconds per tick.
func etaMinutes(tick, total int, intervalSec float64) float64 {
	remaining := total - tick
	if remaining <= 0 {
		return 0
	}
	return float64(remaining) * intervalSec / 60.0
}

// jitter returns a random value in [-spread/2, +spread/2]. Used throughout
// scenarios to add realism to speed and heading values. math/rand is
// intentional — simulation data does not require cryptographic randomness.
func jitter(spread float64) float64 {
	// #nosec G404 -- simulation jitter, not security-sensitive
	return rand.Float64()*spread - spread/2 //nolint:gosec // simulation jitter
}
