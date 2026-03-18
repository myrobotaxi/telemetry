package simulator

// parkingLot simulates rapid gear transitions (P->D->R->P) for edge-case
// testing of drive detection.
type parkingLot struct {
	state       ScenarioState
	tick        int
	total       int
	gearPattern []gearStep
	stepIdx     int
}

type gearStep struct {
	gear     string
	speed    float64
	duration int
}

func newParkingLot() *parkingLot {
	return &parkingLot{
		state: scenarioDefaults(),
		total: 120, // 2 minutes of rapid gear changes
		gearPattern: []gearStep{
			{"P", 0, 3},
			{"D", 5, 5},
			{"P", 0, 2},
			{"R", 3, 4},
			{"P", 0, 2},
			{"D", 8, 6},
			{"P", 0, 3},
			{"R", 5, 5},
			{"D", 3, 3},
			{"P", 0, 4},
			{"D", 10, 8},
			{"P", 0, 3},
		},
	}
}

func (p *parkingLot) Name() string { return "parking-lot" }
func (p *parkingLot) Done() bool   { return p.tick >= p.total }

func (p *parkingLot) Next() ScenarioState {
	defer func() { p.tick++ }()

	p.advanceGearPattern()
	step := p.gearPattern[p.stepIdx%len(p.gearPattern)]
	p.state.GearPosition = step.gear
	p.state.Speed = step.speed + jitter(2)
	if step.gear == "P" {
		p.state.Speed = 0
	}

	// Reverse heading when in reverse.
	if step.gear == "R" {
		p.state.Heading = normalizeHeading(p.state.Heading + 180)
	}

	p.state.Latitude, p.state.Longitude = advancePosition(
		p.state.Latitude, p.state.Longitude,
		p.state.Heading, p.state.Speed, 1.0,
	)

	return p.state
}

func (p *parkingLot) advanceGearPattern() {
	idx := p.stepIdx % len(p.gearPattern)
	p.gearPattern[idx].duration--
	if p.gearPattern[idx].duration <= 0 {
		p.stepIdx++
	}
}
