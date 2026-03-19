package telemetry

// Tesla Fleet API field names. These are the exact strings that the
// Fleet API expects in the "fields" map of a telemetry config request.
// They differ from our internal FieldName constants which are
// camelCase domain names.
const (
	FleetFieldVehicleSpeed    = "VehicleSpeed"
	FleetFieldLocation        = "Location"
	FleetFieldHeading         = "Heading"
	FleetFieldGear            = "Gear"
	FleetFieldSOC             = "Soc"
	FleetFieldEstBatteryRange = "EstBatteryRange"
	FleetFieldChargeState     = "ChargeState"
	FleetFieldOdometer        = "Odometer"
	FleetFieldInsideTemp      = "InsideTemp"
	FleetFieldOutsideTemp     = "OutsideTemp"
)

// DefaultFieldConfig returns the standard set of telemetry fields and
// intervals that MyRoboTaxi configures on each vehicle. These intervals
// balance data freshness against the vehicle's 5000-message buffer.
//
// Tesla's emission rule: a field is only emitted when BOTH the interval
// has elapsed AND the value has changed since the last emission.
func DefaultFieldConfig() map[string]FieldConfig {
	locationDelta := float64(10) // meters; filters out GPS jitter while parked

	return map[string]FieldConfig{
		FleetFieldVehicleSpeed:    {IntervalSeconds: 2},
		FleetFieldLocation:        {IntervalSeconds: 2, MinimumDelta: &locationDelta},
		FleetFieldHeading:         {IntervalSeconds: 5},
		FleetFieldGear:            {IntervalSeconds: 1},
		FleetFieldSOC:             {IntervalSeconds: 30},
		FleetFieldEstBatteryRange: {IntervalSeconds: 30},
		FleetFieldChargeState:     {IntervalSeconds: 30},
		FleetFieldOdometer:        {IntervalSeconds: 60},
		FleetFieldInsideTemp:      {IntervalSeconds: 60},
		FleetFieldOutsideTemp:     {IntervalSeconds: 60},
	}
}
