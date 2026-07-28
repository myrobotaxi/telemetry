package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
)

// VehicleData is the SUBSET of Tesla's GET /api/1/vehicles/{vin}/vehicle_data
// response that MyRoboTaxi reads to populate the stream-only owner-control
// fields (Lock / Trunk / Climate / Charge / Odometer / Temps) for a vehicle
// that is NOT streaming — in service, asleep, or offline (MYR-260). Those
// fields are otherwise stream-fed only, so the app shows "— Syncing" forever
// when the car does not stream.
//
// Only the four sub-objects and the specific fields that map onto the wire
// are decoded. The full vehicle_data payload also carries live GPS
// (drive_state.latitude/longitude) which is P1 — the raw payload is NEVER
// logged (data-classification.md), so this struct exists purely to pluck the
// non-identifying control fields we need.
//
// Each field is a pointer so an absent field decodes to nil and is skipped by
// the mapper rather than being written as a misleading zero value.
type VehicleData struct {
	VehicleState  *VehicleDataVehicleState  `json:"vehicle_state"`
	ClimateState  *VehicleDataClimateState  `json:"climate_state"`
	ChargeState   *VehicleDataChargeState   `json:"charge_state"`
	VehicleConfig *VehicleDataVehicleConfig `json:"vehicle_config"`
}

// VehicleDataVehicleState is the vehicle_state sub-object subset: lock state,
// front/rear trunk positions (0 = closed, non-zero = open), and odometer.
type VehicleDataVehicleState struct {
	Locked     *bool    `json:"locked"`
	FrontTrunk *int     `json:"ft"`
	RearTrunk  *int     `json:"rt"`
	Odometer   *float64 `json:"odometer"` // miles
	// CarVersion is the installed Tesla firmware string (e.g. "2026.20.1 9a8b").
	// MYR-279: the non-streaming counterpart of the streamed Version proto field
	// so a car that is not streaming still surfaces its software version on the
	// /snapshot. Absent field decodes to nil and is skipped by the mapper.
	CarVersion *string `json:"car_version"`
}

// VehicleDataVehicleConfig is the vehicle_config sub-object subset: the trim
// badge, the human-readable trim label, the exterior colour and the
// ventilated-seat capability. None is a streamed telemetry field, so this REST
// read is their ONLY source. Absent fields decode to nil and are skipped by the
// mapper.
//
// MYR-279: trim is the badge string (e.g. "Performance", "Long Range").
//
// MYR-320: PerformancePackage is the HUMAN-READABLE trim/performance
// designation (live-verified: "Performance" on the client's own car). It is the
// display-safe sibling of TrimBadging, which carries the RAW BADGE CODE (e.g.
// "p74d") and is explicitly NOT display-safe. Both are kept — neither replaces
// the other — and only the label reaches a consumer as `trimLabel`.
//
// MYR-320: ExteriorColor is Tesla's own colour name (live-verified:
// "Quicksilver"). Unlike every other field on this struct it does NOT travel as
// a telemetry field: the wire already exposes `color`, emitted straight from the
// Prisma-owned "Vehicle".color column, which nothing has ever populated. So the
// decode feeds a narrow application-runtime UPDATE of that column
// (store.VehicleRepo.UpdateVehicleColor, data-lifecycle.md §1.4) and the value
// then flows to both the snapshot and the vehicles-list with ZERO contract
// change. An EMPTY string is never written: it would blank a colour a human
// (or an earlier read) already got right.
//
// MYR-308: HasSeatCooling is whether the car is EQUIPPED WITH ventilated (cooled)
// front seats — a SPEC fact, not a runtime state. Contrast the streamed
// SeatVentEnabled (proto 254), which is the on/off of that equipment: a car can
// be has_seat_cooling=true with seatVentEnabled=false. Verified live against the
// client's vehicle, which returns true. A nil here means Tesla omitted the key
// (older firmware, or a partial payload) and MUST NOT be read as "no seat
// cooling" — the mapper skips it so the column stays NULL and clients keep the
// pre-MYR-308 telemetry-presence heuristic. Only an explicit false is the
// authoritative no.
type VehicleDataVehicleConfig struct {
	TrimBadging        *string `json:"trim_badging"`
	PerformancePackage *string `json:"performance_package"`
	ExteriorColor      *string `json:"exterior_color"`
	HasSeatCooling     *bool   `json:"has_seat_cooling"`
}

// VehicleDataClimateState is the climate_state sub-object subset: whether the
// climate system is on, plus cabin/ambient temperatures. Tesla reports
// inside_temp/outside_temp in CELSIUS; the mapper converts to Fahrenheit to
// match the wire contract (vehicle-state-schema.md §1.1).
type VehicleDataClimateState struct {
	IsClimateOn *bool    `json:"is_climate_on"`
	InsideTemp  *float64 `json:"inside_temp"`  // Celsius
	OutsideTemp *float64 `json:"outside_temp"` // Celsius
}

// VehicleDataChargeState is the charge_state sub-object subset: the charging
// state enum (values match Tesla proto 179 DetailedChargeState strings), the
// charge-port door position, and the battery level.
type VehicleDataChargeState struct {
	ChargingState      *string `json:"charging_state"`
	ChargePortDoorOpen *bool   `json:"charge_port_door_open"`
	BatteryLevel       *int    `json:"battery_level"` // percent
}

// vehicleDataResponse mirrors GET /api/1/vehicles/{vin}/vehicle_data.
type vehicleDataResponse struct {
	Response VehicleData `json:"response"`
}

// GetVehicleData reads a vehicle's full-state REST object from the Fleet API
// (GET /api/1/vehicles/{vin}/vehicle_data) and returns the control-field
// subset MyRoboTaxi surfaces to owners (MYR-260). Like GetVehicle this is an
// UNSIGNED authenticated read against the DIRECT Fleet API base URL (NOT the
// signing tesla-http-proxy); the token must be a valid OAuth2 Bearer token for
// the vehicle's owner, and the vehicle_device_data scope must be granted.
//
// This is a SINGLE read. It does NOT force-wake the vehicle: if the car is
// asleep or offline Tesla returns 408 (or another non-2xx), which doWithRetry
// does not retry (408 is not in isRetryable) — the caller treats any error as
// a non-fatal skip. The success payload contains GPS (P1); it is decoded but
// the raw bytes are NEVER logged, and decode errors never embed the body.
func (c *FleetAPIClient) GetVehicleData(
	ctx context.Context,
	token string,
	vin string,
) (*VehicleData, error) {
	if token == "" {
		return nil, fmt.Errorf("GetVehicleData: auth token is required")
	}
	if len(vin) != vinLength {
		return nil, fmt.Errorf("GetVehicleData: invalid VIN %q (must be 17 characters)", redactVIN(vin))
	}

	url := c.baseURL + "/api/1/vehicles/" + neturl.PathEscape(vin) + "/vehicle_data"

	c.logger.Debug("fetching vehicle_data rest object", slog.String("vin", redactVIN(vin)))

	respBody, err := c.doWithRetry(ctx, http.MethodGet, url, token, nil)
	if err != nil {
		return nil, fmt.Errorf("GetVehicleData(%s): %w", redactVIN(vin), err)
	}

	var result vehicleDataResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		// Deliberately DO NOT include respBody in the error — the vehicle_data
		// payload carries GPS (P1). Only the redacted VIN + decode cause leak.
		return nil, fmt.Errorf("GetVehicleData(%s): decode response: %w", redactVIN(vin), err)
	}

	return &result.Response, nil
}
