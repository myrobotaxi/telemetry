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
	FleetFieldSOC               = "Soc"
	FleetFieldBatteryLevel      = "BatteryLevel"
	FleetFieldEstBatteryRange   = "EstBatteryRange"
	FleetFieldIdealBatteryRange = "IdealBatteryRange"
	FleetFieldRatedRange        = "RatedRange"
	FleetFieldEnergyRemaining   = "EnergyRemaining"
	FleetFieldPackVoltage       = "PackVoltage"
	FleetFieldPackCurrent       = "PackCurrent"
	// MYR-42: FleetFieldChargeState (proto 2) removed from DefaultFieldConfig
	// because Tesla firmware no longer populates it. chargeState wire field
	// now sources from DetailedChargeState (proto 179).
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
	// MYR-252 Group B cabin-control read-back fields (added to the
	// fleet-telemetry config + server field map so Tesla emits them).
	FleetFieldHvacAutoMode         = "HvacAutoMode"
	FleetFieldHvacACEnabled        = "HvacACEnabled"
	FleetFieldSeatHeaterRearLeft   = "SeatHeaterRearLeft"
	FleetFieldSeatHeaterRearCenter = "SeatHeaterRearCenter"
	FleetFieldSeatHeaterRearRight  = "SeatHeaterRearRight"
	FleetFieldSeatCoolerLeft       = "ClimateSeatCoolingFrontLeft"
	FleetFieldSeatCoolerRight      = "ClimateSeatCoolingFrontRight"
	FleetFieldSeatVentEnabled      = "SeatVentEnabled"
)

// --- Vehicle State ---

const (
	FleetFieldOdometer           = "Odometer"
	FleetFieldVehicleName        = "VehicleName"
	FleetFieldCarType            = "CarType"
	FleetFieldVersion            = "Version"
	FleetFieldLocked             = "Locked"
	FleetFieldSentryMode         = "SentryMode"
	FleetFieldChargePortDoorOpen = "ChargePortDoorOpen" // MYR-252 Group B
	FleetFieldDoorState          = "DoorState"          // MYR-252 Group B (trunk/frunk decode)
	FleetFieldServiceMode        = "ServiceMode"        // MYR-259 proto 159 — in_service status source
)

// --- Media (MYR-252 Group B) ---

const (
	FleetFieldMediaPlaybackStatus = "MediaPlaybackStatus"
	FleetFieldMediaVolume         = "MediaAudioVolume"
)

// --- Media now-playing (MYR-303) ---
//
// The now-playing block: what is playing (title/artist/album), where it is
// playing from (station = the channel WITHIN a source; playback source = the
// app/input doing the playing), how long it is (duration/elapsed), and the
// per-vehicle volume ceiling. Proto numbers verified against the vendored
// internal/telemetry/proto/tesla/vehicle_data.proto:
//
//	MediaPlaybackSource     = 243
//	MediaNowPlayingDuration = 245
//	MediaNowPlayingElapsed  = 246
//	MediaNowPlayingArtist   = 247
//	MediaNowPlayingTitle    = 248
//	MediaNowPlayingAlbum    = 249
//	MediaNowPlayingStation  = 250
//	MediaAudioVolumeMax     = 252
//
// Note the two name contractions applied at the fieldMap boundary (fields.go),
// NOT in the WS translate table: MediaAudioVolumeMax → wire `mediaVolumeMax`,
// exactly as MYR-252 contracted MediaAudioVolume (244) → `mediaVolume`; and
// MediaNowPlayingDuration/Elapsed pick up an explicit `Ms` suffix on the wire
// because the contract fixes the unit at milliseconds.
const (
	FleetFieldMediaPlaybackSource = "MediaPlaybackSource"
	FleetFieldMediaTitle          = "MediaNowPlayingTitle"
	FleetFieldMediaArtist         = "MediaNowPlayingArtist"
	FleetFieldMediaAlbum          = "MediaNowPlayingAlbum"
	FleetFieldMediaStation        = "MediaNowPlayingStation"
	FleetFieldMediaDuration       = "MediaNowPlayingDuration"
	FleetFieldMediaElapsed        = "MediaNowPlayingElapsed"
	FleetFieldMediaVolumeMax      = "MediaAudioVolumeMax"
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
		FleetFieldDetailedChargeState:               {IntervalSeconds: 30}, // proto 179 — sources the `chargeState` wire field as of MYR-42 (2026-04-23)
		FleetFieldTimeToFullCharge:                  {IntervalSeconds: 30}, // proto 43, hours (decimal double) — v1 charge atomic group member
		FleetFieldEstimatedHoursToChargeTermination: {IntervalSeconds: 30}, // MYR-25 observation: proto 190, MYR-28 flip-condition guard

		// Climate. InsideTemp at 10s so the owner sees the cabin temperature
		// change as climate runs (MYR-276: client saw it lag ~a minute at 60s —
		// interior temp moves visibly while cooling/heating). OutsideTemp stays
		// low-frequency (ambient changes slowly).
		FleetFieldInsideTemp:  {IntervalSeconds: 10, ResendIntervalSeconds: intPtr(120)},
		FleetFieldOutsideTemp: {IntervalSeconds: 60, ResendIntervalSeconds: intPtr(120)},
		// MYR-300: HvacPower MUST carry a resend. Tesla emits it on change
		// only, so a server that reconnects while the car is already cooling
		// never re-learns that climate is on — `isClimateOn` then reads Off
		// durably while the car's own screen says "Cooling Down". The 120s
		// resend matches the sibling comfort fields above and is what the
		// MYR-300 backfill freshness window (defaultStreamFreshness) is sized
		// against. NOTE: a config change only reaches a car on a re-push
		// (POST /api/fleet-config/{vin}, `ops fleet-config push`, or the next
		// owner link) — there is no config version/hash that re-pushes itself.
		FleetFieldHvacPower:            {IntervalSeconds: 10, ResendIntervalSeconds: intPtr(120)},
		FleetFieldHvacFanSpeed:         {IntervalSeconds: 30},
		FleetFieldDriverTempSetting:    {IntervalSeconds: 30},
		FleetFieldPassengerTempSetting: {IntervalSeconds: 30},
		FleetFieldDefrostMode:          {IntervalSeconds: 30},
		FleetFieldSeatHeaterLeft:       {IntervalSeconds: 30},
		FleetFieldSeatHeaterRight:      {IntervalSeconds: 30},
		FleetFieldClimateKeeperMode:    {IntervalSeconds: 60},
		// MYR-252 Group B — cabin comfort read-back. Low-churn comfort
		// state; ResendIntervalSeconds re-warms the value after a
		// parked-window reconnect (Tesla only emits on change).
		FleetFieldHvacAutoMode:         {IntervalSeconds: 30, ResendIntervalSeconds: intPtr(120)},
		FleetFieldHvacACEnabled:        {IntervalSeconds: 30, ResendIntervalSeconds: intPtr(120)},
		FleetFieldSeatHeaterRearLeft:   {IntervalSeconds: 30},
		FleetFieldSeatHeaterRearCenter: {IntervalSeconds: 30},
		FleetFieldSeatHeaterRearRight:  {IntervalSeconds: 30},
		// MYR-299: the seat-cooler fields MUST carry a resend, because their
		// PRESENCE is the ventilated-seat capability signal the client gates
		// the Heat/Cool toggle on. A car without cooled seats never emits
		// protos 237/238 at all; a car with them emits a value including 0
		// (present-but-off). Tesla emits them on change only, so without a
		// resend a vented car that has not touched its seat coolers since the
		// last (re)connect looks identical to a car that has none — and the
		// owner is locked out of Cool. The 120s resend matches the sibling
		// comfort fields and re-asserts presence continuously, which is what
		// makes ABSENCE meaningful. NOTE: a config change only reaches a car
		// on a re-push (POST /api/fleet-config/{vin}, `ops fleet-config push`,
		// or the next owner link) — there is no config version/hash that
		// re-pushes itself.
		FleetFieldSeatCoolerLeft:  {IntervalSeconds: 30, ResendIntervalSeconds: intPtr(120)},
		FleetFieldSeatCoolerRight: {IntervalSeconds: 30, ResendIntervalSeconds: intPtr(120)},
		FleetFieldSeatVentEnabled: {IntervalSeconds: 30, ResendIntervalSeconds: intPtr(120)},

		// Vehicle state — low frequency
		//
		// Odometer runs on the tighter 15s counter cadence (MYR-158):
		// drive distance derives from the odometer delta (MYR-157), so
		// the start/end baselines must be sampled close to the true
		// drive boundaries or the final stretch of every drive is
		// missed. The resend re-warms the cache after a parked-window
		// (re)connect, mirroring the mileage counters below.
		FleetFieldOdometer:    {IntervalSeconds: 15, ResendIntervalSeconds: intPtr(15)},
		FleetFieldVehicleName: {IntervalSeconds: 300}, // Received for potential sync but NOT broadcast to SDK clients (MYR-30). SDK name comes from DB Vehicle.name.
		FleetFieldCarType:     {IntervalSeconds: 300},
		FleetFieldVersion:     {IntervalSeconds: 300},
		FleetFieldLocked:      {IntervalSeconds: 30},
		FleetFieldSentryMode:  {IntervalSeconds: 30},
		// MYR-252 Group B — door/charge-port state. Resend re-warms after
		// a parked-window reconnect so a client that connects to a static
		// car still learns whether the trunk/frunk/charge-port is open.
		FleetFieldChargePortDoorOpen: {IntervalSeconds: 30, ResendIntervalSeconds: intPtr(120)},
		FleetFieldDoorState:          {IntervalSeconds: 30, ResendIntervalSeconds: intPtr(120)},

		// MYR-259: ServiceMode (proto 159, bool). A vehicle enters/exits
		// service mode rarely, so a comfort-ish 60s interval is plenty; the
		// 300s resend re-warms the value after a parked-window reconnect
		// (Tesla only emits on change) so a server that missed the initial
		// emission still learns the car is in service before the next toggle.
		// This is the LIVE signal for status=in_service; the REST
		// in_service read (connectivity-edge) is the authoritative persist.
		FleetFieldServiceMode: {IntervalSeconds: 60, ResendIntervalSeconds: intPtr(300)},

		// Media (MYR-252 Group B) — higher churn than comfort state, so a
		// tighter interval; no resend (a stale media state self-corrects on
		// the next play/pause/volume change).
		FleetFieldMediaPlaybackStatus: {IntervalSeconds: 10},
		FleetFieldMediaVolume:         {IntervalSeconds: 10},

		// Media now-playing (MYR-303). Same 10s interval as the two MYR-252
		// media siblings above — Tesla emits the Media group on change, and a
		// track change is exactly the kind of event the owner expects to see
		// reflected promptly.
		//
		// Unlike those two siblings these DO carry a ResendIntervalSeconds, per
		// the MYR-300 lesson: Tesla emits on change ONLY, so a server that
		// (re)connects mid-track never re-learns the now-playing block and the
		// panel reads empty while the car's own screen shows the track. That is
		// the same "server that misses the initial emission" failure MYR-300 hit
		// with HvacPower and MYR-299 hit with the seat coolers. The MYR-252
		// siblings' "a stale media state self-corrects on the next play/pause"
		// reasoning does NOT carry over: a paused car mid-album can sit
		// unchanged for hours, and the volume CEILING (mediaVolumeMax) is
		// near-constant per vehicle, so without a resend it may literally never
		// be emitted again after the first connect — leaving every volume
		// percentage the client renders scaled against a guessed 11.
		//
		// 120s matches the sibling comfort/seat-cooler resends and is the window
		// defaultStreamFreshness (service_status_stream_freshness.go) is sized
		// against, so these fields keep the MYR-300 backfill gate coherent.
		//
		// NOTE: a config change only reaches a car on a re-push (POST
		// /api/fleet-config/{vin}, `ops fleet-config push`, or the next owner
		// link) — there is no config version/hash that re-pushes itself. The
		// eight fields below are NEW subscriptions, so every already-linked VIN
		// needs that re-push before it will emit them at all.
		FleetFieldMediaPlaybackSource: {IntervalSeconds: 10, ResendIntervalSeconds: intPtr(120)},
		FleetFieldMediaTitle:          {IntervalSeconds: 10, ResendIntervalSeconds: intPtr(120)},
		FleetFieldMediaArtist:         {IntervalSeconds: 10, ResendIntervalSeconds: intPtr(120)},
		FleetFieldMediaAlbum:          {IntervalSeconds: 10, ResendIntervalSeconds: intPtr(120)},
		FleetFieldMediaStation:        {IntervalSeconds: 10, ResendIntervalSeconds: intPtr(120)},
		FleetFieldMediaDuration:       {IntervalSeconds: 10, ResendIntervalSeconds: intPtr(120)},
		FleetFieldMediaElapsed:        {IntervalSeconds: 10, ResendIntervalSeconds: intPtr(120)},
		FleetFieldMediaVolumeMax:      {IntervalSeconds: 10, ResendIntervalSeconds: intPtr(120)},

		// Safety / ADAS.
		//
		// MYR-155: these two are cumulative "miles since reset" counters
		// gated by MinimumDelta (1 mile), so they ONLY emit while the value
		// is changing — i.e. while driving. They go silent the moment the
		// car parks. A server that (re)starts while a vehicle is parked
		// therefore never receives an FSD value until that car drives a full
		// FSD mile, leaving the drive detector with no FSD baseline at the
		// start of the first post-restart drive (it records 0). The
		// ResendIntervalSeconds forces a periodic re-emit even when
		// unchanged — exactly the "server that misses the initial emission"
		// case the file header calls out — so the cache (and the snapshot)
		// re-warm quickly after any reconnect, before a drive begins.
		//
		// MYR-158: cadence tightened 60s → 15s (with Odometer above) so
		// per-drive absolute FSD-miles/distance baselines are sampled
		// within ~15s of the true drive boundaries instead of up to
		// ~60s away. MinimumDelta MUST stay 1 — Tesla requires
		// minimum_delta >= 1 for mileage fields, so cadence is the only
		// tunable lever. NOTE: takes effect per vehicle only after a
		// fleet-wide config re-push (POST /api/fleet-config/{vin}).
		FleetFieldMilesSinceReset:    {IntervalSeconds: 15, MinimumDelta: &oneMile, ResendIntervalSeconds: intPtr(15)},
		FleetFieldFSDMilesSinceReset: {IntervalSeconds: 15, MinimumDelta: &oneMile, ResendIntervalSeconds: intPtr(15)},
	}
}
