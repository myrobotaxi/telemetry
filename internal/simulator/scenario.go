// Package simulator provides a mock Tesla vehicle that sends protobuf
// telemetry to the server, enabling pipeline testing without a real car.
package simulator

// Scenario generates a sequence of telemetry states for a simulated drive.
// Implementations advance through their route on each call to Next.
type Scenario interface {
	// Name returns the human-readable scenario identifier.
	Name() string

	// Next returns the current telemetry state and advances the scenario.
	Next() ScenarioState

	// Done reports whether the scenario has completed its route.
	Done() bool
}

// ScenarioState holds the telemetry values for a single tick of a simulated
// drive. These map directly to Tesla proto fields that the receiver decodes.
type ScenarioState struct {
	Speed          float64 // mph
	Latitude       float64
	Longitude      float64
	Heading        float64 // degrees, 0-360
	GearPosition   string  // "P", "D", "R", "N"
	ChargeLevel    int     // percent 0-100
	EstimatedRange int     // miles
	InteriorTemp   int     // celsius
	ExteriorTemp   int     // celsius
	OdometerMiles  float64
}
