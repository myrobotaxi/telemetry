package store

import "time"

// controlStateScan holds the nullable go_vehicle_control_state columns that the
// GetByID snapshot read (queryVehicleByID, queries.go) LEFT JOINs onto the base
// "Vehicle" row. Every field is a pointer: a NULL column — no side-table row at
// all, or a field never observed for this car — scans into nil, which the
// snapshot surfaces as an honest null rather than a fabricated value.
//
// This is deliberately a plain destination bag rather than a domain type (so the
// "no God structs" rule in CLAUDE.md does not apply): it exists so scanVehicleByID
// stays a three-liner instead of growing another three lines — a var, a scan
// destination, and an assignment — every time a column is added to the side
// table. That growth is what pushed the old inline form past the funlen cap when
// MYR-303/308 added nine columns.
//
// The dests() order MUST match the column order in queryVehicleByID exactly.
type controlStateScan struct {
	// MYR-269 owner-control booleans.
	isLocked       *bool
	frunkOpen      *bool
	trunkOpen      *bool
	isClimateOn    *bool
	chargePortOpen *bool

	// MYR-273 cabin-setting levels.
	driverTemp           *int
	passengerTemp        *int
	fanSpeed             *int
	seatHeaterLeft       *int
	seatHeaterRight      *int
	seatHeaterRearLeft   *int
	seatHeaterRearCenter *int
	seatHeaterRearRight  *int
	seatCoolerLeft       *int
	seatCoolerRight      *int
	mediaVolume          *float64

	// MYR-279 vehicle-detail strings.
	softwareVersion *string
	trim            *string

	// MYR-274 climate-mode read-backs.
	hvacAutoMode  *string
	hvacAcEnabled *bool

	// MYR-298 seat-vent + media-playback read-backs.
	seatVentEnabled     *bool
	mediaPlaybackStatus *string

	// MYR-303 media now-playing block. For the five text fields nil means NEVER
	// OBSERVED, which is distinct from a non-nil empty string ("observed, and
	// nothing is playing") — the divergence documented in
	// mapControlMediaNowPlaying.
	mediaTitle    *string
	mediaArtist   *string
	mediaAlbum    *string
	mediaStation  *string
	mediaSource   *string
	mediaDuration *int64
	mediaElapsed  *int64
	mediaVolMax   *float64

	// MYR-308 ventilated-seat capability (REST-sourced spec fact). nil means
	// never read, NOT "no seat cooling".
	seatCoolingCapable *bool

	// MYR-316 service window, the two independent sources behind the single
	// wire field serviceEstimatedEndAt. serviceETC is Tesla's own estimate
	// (service_data.service_etc) and WINS; serviceExpectedEndAt is the
	// owner-entered fallback. nil means "no estimate from this source" — for
	// serviceETC that is common and normal, because Tesla returns an all-null
	// service_data body for a visit with no appointment record.
	serviceETC           *time.Time
	serviceExpectedEndAt *time.Time
}

// dests returns the scan destinations in queryVehicleByID's column order, for
// use as scanVehicleRowExtra's trailing `extra` arguments.
func (c *controlStateScan) dests() []any {
	return []any{
		&c.isLocked, &c.frunkOpen, &c.trunkOpen, &c.isClimateOn, &c.chargePortOpen,
		&c.driverTemp, &c.passengerTemp, &c.fanSpeed,
		&c.seatHeaterLeft, &c.seatHeaterRight,
		&c.seatHeaterRearLeft, &c.seatHeaterRearCenter, &c.seatHeaterRearRight,
		&c.seatCoolerLeft, &c.seatCoolerRight, &c.mediaVolume,
		&c.softwareVersion, &c.trim,
		&c.hvacAutoMode, &c.hvacAcEnabled,
		&c.seatVentEnabled, &c.mediaPlaybackStatus,
		&c.mediaTitle, &c.mediaArtist, &c.mediaAlbum,
		&c.mediaStation, &c.mediaSource,
		&c.mediaDuration, &c.mediaElapsed, &c.mediaVolMax,
		&c.seatCoolingCapable,
		&c.serviceETC, &c.serviceExpectedEndAt,
	}
}

// applyTo copies the scanned control-state columns onto the Vehicle. Kept
// separate from dests() so the column order (which must track the SQL) and the
// field mapping (which must track the domain type) fail independently and
// loudly rather than silently transposing two same-typed columns.
func (c *controlStateScan) applyTo(v *Vehicle) {
	v.IsLocked = c.isLocked
	v.FrunkOpen = c.frunkOpen
	v.TrunkOpen = c.trunkOpen
	v.IsClimateOn = c.isClimateOn
	v.ChargePortOpen = c.chargePortOpen
	v.DriverTempSetting = c.driverTemp
	v.PassengerTempSetting = c.passengerTemp
	v.FanSpeed = c.fanSpeed
	v.SeatHeaterLeft = c.seatHeaterLeft
	v.SeatHeaterRight = c.seatHeaterRight
	v.SeatHeaterRearLeft = c.seatHeaterRearLeft
	v.SeatHeaterRearCenter = c.seatHeaterRearCenter
	v.SeatHeaterRearRight = c.seatHeaterRearRight
	v.SeatCoolerLeft = c.seatCoolerLeft
	v.SeatCoolerRight = c.seatCoolerRight
	v.MediaVolume = c.mediaVolume
	v.SoftwareVersion = c.softwareVersion
	v.Trim = c.trim
	v.HvacAutoMode = c.hvacAutoMode
	v.HvacAcEnabled = c.hvacAcEnabled
	v.SeatVentEnabled = c.seatVentEnabled
	v.MediaPlaybackStatus = c.mediaPlaybackStatus
	v.MediaNowPlayingTitle = c.mediaTitle
	v.MediaNowPlayingArtist = c.mediaArtist
	v.MediaNowPlayingAlbum = c.mediaAlbum
	v.MediaNowPlayingStation = c.mediaStation
	v.MediaPlaybackSource = c.mediaSource
	v.MediaNowPlayingDuration = c.mediaDuration
	v.MediaNowPlayingElapsed = c.mediaElapsed
	v.MediaVolumeMax = c.mediaVolMax
	v.SeatCoolingCapable = c.seatCoolingCapable
	v.ServiceETC = c.serviceETC
	v.ServiceExpectedEndAt = c.serviceExpectedEndAt
}
