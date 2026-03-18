package simulator

import (
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
func NewScenario(name string) Scenario {
	switch name {
	case "highway-drive":
		return newHighwayDrive()
	case "city-drive":
		return newCityDrive()
	case "parking-lot":
		return newParkingLot()
	default:
		return nil
	}
}

// ScenarioNames returns the list of supported scenario names.
func ScenarioNames() []string {
	return []string{"highway-drive", "city-drive", "parking-lot"}
}

// --- Highway Drive ---

// highwayDrive simulates a ~30-minute highway trip: park -> accelerate -> cruise
// at 65-75 mph -> decelerate -> park.
type highwayDrive struct {
	state ScenarioState
	tick  int
	total int // total ticks (1 tick = 1 second at default interval)
}

func newHighwayDrive() *highwayDrive {
	return &highwayDrive{
		state: scenarioDefaults(),
		total: 1800, // 30 minutes
	}
}

func (h *highwayDrive) Name() string { return "highway-drive" }
func (h *highwayDrive) Done() bool   { return h.tick >= h.total }

func (h *highwayDrive) Next() ScenarioState {
	defer func() { h.tick++ }()

	h.updateGear()
	h.updateSpeed()
	h.updatePosition()
	h.drainCharge()

	return h.state
}

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
	// Small heading drift for realism.
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

// --- City Drive ---

// cityDrive simulates a ~15-minute city drive with stop-and-go segments.
type cityDrive struct {
	state    ScenarioState
	tick     int
	total    int
	segments []segment
	segIdx   int
}

type segment struct {
	targetSpeed float64
	duration    int
}

func newCityDrive() *cityDrive {
	return &cityDrive{
		state: scenarioDefaults(),
		total: 900, // 15 minutes
		segments: []segment{
			{0, 5},    // parked start
			{25, 60},  // residential
			{0, 30},   // stop light
			{35, 90},  // main road
			{0, 20},   // stop sign
			{30, 80},  // side street
			{0, 25},   // red light
			{35, 120}, // arterial
			{0, 15},   // stop
			{20, 60},  // neighborhood
			{0, 5},    // park
		},
	}
}

func (c *cityDrive) Name() string { return "city-drive" }
func (c *cityDrive) Done() bool   { return c.tick >= c.total }

func (c *cityDrive) Next() ScenarioState {
	defer func() { c.tick++ }()

	c.advanceSegment()
	c.applySegment()
	c.updateCityPosition()
	c.drainCityCharge()

	return c.state
}

func (c *cityDrive) advanceSegment() {
	if c.segIdx >= len(c.segments) {
		return
	}
	c.segments[c.segIdx].duration--
	if c.segments[c.segIdx].duration <= 0 && c.segIdx < len(c.segments)-1 {
		c.segIdx++
	}
}

func (c *cityDrive) applySegment() {
	if c.segIdx >= len(c.segments) {
		c.state.Speed = 0
		c.state.GearPosition = "P"
		return
	}
	seg := c.segments[c.segIdx]
	if seg.targetSpeed == 0 {
		c.state.Speed = clampSpeed(c.state.Speed-3, 0, 45)
		if c.state.Speed == 0 {
			c.state.GearPosition = "P"
		}
	} else {
		c.state.GearPosition = "D"
		diff := seg.targetSpeed - c.state.Speed
		c.state.Speed = clampSpeed(c.state.Speed+clampSpeed(diff*0.3, -5, 5), 0, 45)
	}
}

func (c *cityDrive) updateCityPosition() {
	// Heading drift for city turns.
	c.state.Heading = normalizeHeading(c.state.Heading + jitter(6))
	c.state.Latitude, c.state.Longitude = advancePosition(
		c.state.Latitude, c.state.Longitude,
		c.state.Heading, c.state.Speed, 1.0,
	)
	if c.state.Speed > 0 {
		c.state.OdometerMiles += c.state.Speed / 3600.0
	}
}

func (c *cityDrive) drainCityCharge() {
	if c.tick > 0 && c.tick%120 == 0 && c.state.Speed > 0 {
		c.state.ChargeLevel = max(c.state.ChargeLevel-1, 10)
		c.state.EstimatedRange = max(c.state.EstimatedRange-2, 25)
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

// jitter returns a random value in [-spread/2, +spread/2]. Used throughout
// scenarios to add realism to speed and heading values. math/rand is
// intentional — simulation data does not require cryptographic randomness.
func jitter(spread float64) float64 {
	return rand.Float64()*spread - spread/2 //nolint:gosec // simulation data
}
