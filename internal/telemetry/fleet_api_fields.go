package telemetry

// Tesla Fleet API field names. These MUST match the exact string names
// of the Field enum in Tesla's vehicle_data.proto. The authoritative
// source is:
//   https://github.com/teslamotors/fleet-telemetry/blob/main/protos/vehicle_data.proto
//
// Every field in fieldMap (fields.go) must have a corresponding entry
// in DefaultFieldConfig below — otherwise we decode it but never
// request it from the vehicle.

// --- Driving ---

const (
	FleetFieldVehicleSpeed = "VehicleSpeed"
	FleetFieldGear         = "Gear"
	FleetFieldLatAccel     = "LateralAcceleration"
	FleetFieldLongAccel    = "LongitudinalAcceleration"
)

// --- Location / Navigation ---

const (
	FleetFieldLocation        = "Location"
	FleetFieldGpsHeading      = "GpsHeading"
	FleetFieldOriginLocation  = "OriginLocation"
	FleetFieldDestLocation    = "DestinationLocation"
	FleetFieldDestinationName = "DestinationName"
	FleetFieldRouteLine       = "RouteLine"
	// RouteLastUpdated omitted — Tesla docs state this field is broken and never returns data.
	FleetFieldMilesToArrival   = "MilesToArrival"
	FleetFieldMinutesToArrival = "MinutesToArrival"
)

// --- Battery / Charging ---

const (
	FleetFieldSOC                               = "Soc"
	FleetFieldBatteryLevel                      = "BatteryLevel"
	FleetFieldEstBatteryRange                   = "EstBatteryRange"
	FleetFieldIdealBatteryRange                 = "IdealBatteryRange"
	FleetFieldRatedRange                        = "RatedRange"
	FleetFieldEnergyRemaining                   = "EnergyRemaining"
	FleetFieldPackVoltage                       = "PackVoltage"
	FleetFieldPackCurrent                       = "PackCurrent"
	FleetFieldDetailedChargeState               = "DetailedChargeState"
	FleetFieldTimeToFullCharge                  = "TimeToFullCharge"
	FleetFieldEstimatedHoursToChargeTermination = "EstimatedHoursToChargeTermination"
)

// --- Climate ---

const (
	FleetFieldInsideTemp           = "InsideTemp"
	FleetFieldOutsideTemp          = "OutsideTemp"
	FleetFieldHvacPower            = "HvacPower"
	FleetFieldHvacFanSpeed         = "HvacFanSpeed"
	FleetFieldDriverTempSetting    = "HvacLeftTemperatureRequest"
	FleetFieldPassengerTempSetting = "HvacRightTemperatureRequest"
	FleetFieldDefrostMode          = "DefrostMode"
	FleetFieldSeatHeaterLeft       = "SeatHeaterLeft"
	FleetFieldSeatHeaterRight      = "SeatHeaterRight"
	FleetFieldClimateKeeperMode    = "ClimateKeeperMode"
)

// --- Vehicle State ---

const (
	FleetFieldOdometer    = "Odometer"
	FleetFieldVehicleName = "VehicleName"
	FleetFieldCarType     = "CarType"
	FleetFieldVersion     = "Version"
	FleetFieldLocked      = "Locked"
	FleetFieldSentryMode  = "SentryMode"
)

// --- Safety / ADAS ---

const (
	FleetFieldMilesSinceReset    = "MilesSinceReset"
	FleetFieldFSDMilesSinceReset = "SelfDrivingMilesSinceReset"
)

// intPtr returns a pointer to v. Used for optional FieldConfig fields.
func intPtr(v int) *int { return &v }

// DefaultFieldConfig returns the telemetry fields and intervals that
// MyRoboTaxi configures on each vehicle. Every field in fieldMap
// (fields.go) MUST be present here, otherwise we decode it but never
// receive it.
//
// Intervals balance data freshness against the vehicle's 5000-message
// buffer. Tesla's emission rule: a field is only emitted when BOTH the
// interval has elapsed AND the value has changed since the last emission.
//
// Fields that are "set once, static during trip" (navigation endpoints,
// route polyline) use ResendIntervalSeconds so the vehicle re-emits them
// periodically even when the value has not changed. Without this, a
// server that misses the initial emission never receives the data.
func DefaultFieldConfig() map[string]FieldConfig {
	locationDelta := float64(10) // meters; filters out GPS jitter while parked
	oneMile := float64(1)        // Tesla requires minimum_delta >= 1 for mileage fields

	return map[string]FieldConfig{
		// Driving — high frequency
		FleetFieldVehicleSpeed: {IntervalSeconds: 2},
		FleetFieldGear:         {IntervalSeconds: 1},
		FleetFieldLatAccel:     {IntervalSeconds: 2},
		FleetFieldLongAccel:    {IntervalSeconds: 2},

		// Location / Navigation — high frequency with delta filter
		FleetFieldLocation:        {IntervalSeconds: 2, MinimumDelta: &locationDelta},
		FleetFieldGpsHeading:      {IntervalSeconds: 5},
		FleetFieldOriginLocation:  {IntervalSeconds: 1, ResendIntervalSeconds: intPtr(30)},
		FleetFieldDestLocation:    {IntervalSeconds: 1, ResendIntervalSeconds: intPtr(30)},
		FleetFieldDestinationName: {IntervalSeconds: 1, ResendIntervalSeconds: intPtr(30)},
		FleetFieldRouteLine:       {IntervalSeconds: 1, ResendIntervalSeconds: intPtr(30)},
		// RouteLastUpdated omitted — broken per Tesla docs, wastes buffer.
		FleetFieldMilesToArrival:   {IntervalSeconds: 1, ResendIntervalSeconds: intPtr(30)},
		FleetFieldMinutesToArrival: {IntervalSeconds: 1, ResendIntervalSeconds: intPtr(30)},

		// Battery / Charging — medium frequency
		FleetFieldSOC:                               {IntervalSeconds: 30},
		FleetFieldBatteryLevel:                      {IntervalSeconds: 30},
		FleetFieldEstBatteryRange:                   {IntervalSeconds: 30},
		FleetFieldIdealBatteryRange:                 {IntervalSeconds: 30},
		FleetFieldRatedRange:                        {IntervalSeconds: 30},
		FleetFieldEnergyRemaining:                   {IntervalSeconds: 30},
		FleetFieldPackVoltage:                       {IntervalSeconds: 30},
		FleetFieldPackCurrent:                       {IntervalSeconds: 30},
		FleetFieldDetailedChargeState:               {IntervalSeconds: 30}, // maps to FieldDetailedChargeState (proto 179)
		FleetFieldTimeToFullCharge:                  {IntervalSeconds: 30}, // MYR-25: proto 43, unit verification (hours vs seconds)
		FleetFieldEstimatedHoursToChargeTermination: {IntervalSeconds: 30}, // MYR-28: proto 190, delineation vs TimeToFullCharge

		// Climate — medium/low frequency
		FleetFieldInsideTemp:           {IntervalSeconds: 60, ResendIntervalSeconds: intPtr(120)},
		FleetFieldOutsideTemp:          {IntervalSeconds: 60, ResendIntervalSeconds: intPtr(120)},
		FleetFieldHvacPower:            {IntervalSeconds: 10},
		FleetFieldHvacFanSpeed:         {IntervalSeconds: 30},
		FleetFieldDriverTempSetting:    {IntervalSeconds: 30},
		FleetFieldPassengerTempSetting: {IntervalSeconds: 30},
		FleetFieldDefrostMode:          {IntervalSeconds: 30},
		FleetFieldSeatHeaterLeft:       {IntervalSeconds: 30},
		FleetFieldSeatHeaterRight:      {IntervalSeconds: 30},
		FleetFieldClimateKeeperMode:    {IntervalSeconds: 60},

		// Vehicle state — low frequency
		FleetFieldOdometer:    {IntervalSeconds: 60},
		FleetFieldVehicleName: {IntervalSeconds: 300}, // Received for potential sync but NOT broadcast to SDK clients (MYR-30). SDK name comes from DB Vehicle.name.
		FleetFieldCarType:     {IntervalSeconds: 300},
		FleetFieldVersion:     {IntervalSeconds: 300},
		FleetFieldLocked:      {IntervalSeconds: 30},
		FleetFieldSentryMode:  {IntervalSeconds: 30},

		// Safety / ADAS — low frequency
		FleetFieldMilesSinceReset:    {IntervalSeconds: 60, MinimumDelta: &oneMile},
		FleetFieldFSDMilesSinceReset: {IntervalSeconds: 60, MinimumDelta: &oneMile},
	}
}
