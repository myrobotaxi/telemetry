package simulator

// cityDrive simulates a ~15-minute city drive with stop-and-go segments.
// When a route file is loaded, position follows road coordinates.
type cityDrive struct {
	state    ScenarioState
	tick     int
	total    int
	segments []segment
	segIdx   int
	follower *RouteFollower
}

type segment struct {
	targetSpeed float64
	duration    int
}

func newCityDrive(cfg scenarioConfig) *cityDrive {
	c := &cityDrive{
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
	if cfg.routeFile != nil {
		c.follower = NewRouteFollower(cfg.routeFile)
		c.state.Nav.TripDistanceMiles = cfg.routeFile.TotalDistanceMiles
		c.state.Nav.TripDistanceRemain = cfg.routeFile.TotalDistanceMiles
		c.state.Nav.RouteLine = EncodePolyline(cfg.routeFile.Coordinates)
		c.state.Nav.DestinationName = cfg.routeFile.Destination.Name
		c.state.Nav.DestinationLat = cfg.routeFile.Destination.Lat
		c.state.Nav.DestinationLng = cfg.routeFile.Destination.Lng
	}
	return c
}

func (c *cityDrive) Name() string { return "city-drive" }
func (c *cityDrive) Done() bool   { return c.tick >= c.total }

func (c *cityDrive) Next() ScenarioState {
	defer func() { c.tick++ }()

	c.advanceSegment()
	c.applySegment()
	c.updateCityPosition()
	c.drainCityCharge()
	c.state.ETA = etaMinutes(c.tick, c.total, 1.0)

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
	if c.follower != nil {
		c.updateCityPositionFromRoute()
		return
	}
	c.updateCityPositionRandomWalk()
}

func (c *cityDrive) updateCityPositionFromRoute() {
	pos := c.follower.Advance(c.state.Speed, 1.0)
	c.state.Latitude = pos.Lat
	c.state.Longitude = pos.Lng
	c.state.Heading = pos.Heading
	c.state.Nav.TripDistanceRemain = pos.DistanceRemain
	if c.state.Speed > 0 {
		c.state.OdometerMiles += c.state.Speed / 3600.0
	}
}

func (c *cityDrive) updateCityPositionRandomWalk() {
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
