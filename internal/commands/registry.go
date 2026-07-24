package commands

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Command is a registry entry: the mapping from a public command name to
// its Tesla OAuth scope, whether it must be end-to-end signed, and how its
// request parameters are validated and marshalled into the Tesla command
// body. The registry key (and Name) IS the Tesla command name, so the REST
// {name} path segment maps straight through to the Fleet API.
type Command struct {
	// Name is the Tesla command name (== registry key == REST {name}).
	Name string
	// Scope is the Fleet OAuth scope required to invoke it.
	Scope Scope
	// SignerRequired is true when the command must be end-to-end signed with
	// the virtual key. navigation_request/navigation_gps_request are the
	// exceptions: Tesla processes them server-side, so they are forwarded
	// UNSIGNED (see notes below and MYR-176).
	SignerRequired bool
	// BuildBody validates params and returns the Tesla command body JSON.
	// nil means the command takes no parameters (empty body).
	BuildBody func(params map[string]any) ([]byte, error)
}

// Registry is the seeded catalog of commands the P11/P10 surface needs.
type Registry struct {
	cmds map[string]Command
}

// Lookup returns the command by name.
func (r *Registry) Lookup(name string) (Command, bool) {
	c, ok := r.cmds[name]
	return c, ok
}

// Names returns the registered command names in sorted order (for docs and
// tests).
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.cmds))
	for n := range r.cmds {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// NewRegistry builds the default command registry seeded with the P11
// (lock/climate/charge/trunk/horn/lights) and P10 dispatch
// (navigation_request/navigation_gps_request, MYR-176) commands, plus the
// owner-app charge-port, seat-climate, and media commands (MYR-249).
func NewRegistry() *Registry {
	cmds := []Command{
		// --- Doors (P11) ---
		{Name: "door_lock", Scope: ScopeVehicleCmds, SignerRequired: true},
		{Name: "door_unlock", Scope: ScopeVehicleCmds, SignerRequired: true},

		// --- Climate (P11) ---
		{Name: "auto_conditioning_start", Scope: ScopeVehicleCmds, SignerRequired: true},
		{Name: "auto_conditioning_stop", Scope: ScopeVehicleCmds, SignerRequired: true},
		{Name: "set_temps", Scope: ScopeVehicleCmds, SignerRequired: true, BuildBody: buildSetTemps},

		// --- Charging (P11) ---
		{Name: "charge_start", Scope: ScopeChargingCmds, SignerRequired: true},
		{Name: "charge_stop", Scope: ScopeChargingCmds, SignerRequired: true},
		{Name: "set_charge_limit", Scope: ScopeChargingCmds, SignerRequired: true, BuildBody: buildSetChargeLimit},

		// --- Trunk / frunk (P11) ---
		{Name: "actuate_trunk", Scope: ScopeVehicleCmds, SignerRequired: true, BuildBody: buildActuateTrunk},

		// --- Remote start (P11). remote_start_drive is a Fleet vehicle_cmds
		// command; there is no separate "vehicle_remote_start" Fleet scope. ---
		{Name: "remote_start_drive", Scope: ScopeVehicleCmds, SignerRequired: true},

		// --- Horn & lights (P11) ---
		{Name: "honk_horn", Scope: ScopeVehicleCmds, SignerRequired: true},
		{Name: "flash_lights", Scope: ScopeVehicleCmds, SignerRequired: true},

		// --- Navigation / dispatch (P10). UNSIGNED. ONLY navigation_request is
		// usable through the tesla-http-proxy: it processes it server-side and
		// the proxy returns ErrCommandUseRESTAPI, forwarding it to the Fleet
		// REST API (teslamotors/vehicle-command pkg/proxy command.go:545,
		// proxy.go:325). Dispatch (MYR-245) sends navigation_request with a
		// raw "<lat>,<lon>" coordinate-pair share value — text the car's nav
		// geocoder resolves (see internal/dispatch pickupShareValue).
		//
		// navigation_gps_request is NOT forwarded by the proxy: its
		// ExtractCommandAction (command.go) has no case for it, so the proxy
		// hits the default branch and returns HTTP 400 `invalid_command`
		// locally — the car is never dialed (the Jul 18 2026 dispatch outage).
		// It is only reachable via a direct Fleet REST call, which we do not
		// make. It stays registered for the public commands API (a client with
		// its own direct-REST path may still use it), but MUST NOT be used for
		// proxy-routed dispatch. ---
		{Name: "navigation_gps_request", Scope: ScopeVehicleCmds, SignerRequired: false, BuildBody: buildNavigationGPS},
		{Name: "navigation_request", Scope: ScopeVehicleCmds, SignerRequired: false, BuildBody: buildNavigationShare},

		// --- Charge port door (MYR-249). Tesla groups the charge-port door
		// commands under vehicle_charging_cmds alongside charge_start/stop/
		// set_charge_limit. Both are signed (v0.4.1 proxy: ChargePortOpen/
		// ChargePortClose), no params. ---
		{Name: "charge_port_door_open", Scope: ScopeChargingCmds, SignerRequired: true},
		{Name: "charge_port_door_close", Scope: ScopeChargingCmds, SignerRequired: true},

		// --- Seats (MYR-249). Signed vehicle_cmds (v0.4.1 proxy
		// SetSeatHeater/SetSeatCooler). The two commands use DIFFERENT
		// seat_position numbering — see buildSeatHeater/buildSeatCooler. ---
		{Name: "remote_seat_heater_request", Scope: ScopeVehicleCmds, SignerRequired: true, BuildBody: buildSeatHeater},
		{Name: "remote_seat_cooler_request", Scope: ScopeVehicleCmds, SignerRequired: true, BuildBody: buildSeatCooler},

		// --- Media (MYR-249). Signed vehicle_cmds (v0.4.1 proxy
		// ToggleMediaPlayback/MediaNextTrack/MediaPreviousTrack/SetVolume). ---
		{Name: "media_toggle_playback", Scope: ScopeVehicleCmds, SignerRequired: true},
		{Name: "media_next_track", Scope: ScopeVehicleCmds, SignerRequired: true},
		{Name: "media_prev_track", Scope: ScopeVehicleCmds, SignerRequired: true},
		{Name: "adjust_volume", Scope: ScopeVehicleCmds, SignerRequired: true, BuildBody: buildAdjustVolume},
	}

	m := make(map[string]Command, len(cmds))
	for _, c := range cmds {
		m[c.Name] = c
	}
	return &Registry{cmds: m}
}

// marshalBody wraps json.Marshal so BuildBody errors carry package context
// (satisfies the wrapcheck linter; a map[string]any marshal cannot fail in
// practice).
func marshalBody(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal command body: %w", err)
	}
	return b, nil
}

// buildSetTemps marshals {driver_temp, passenger_temp} in Celsius. If
// passenger_temp is omitted it mirrors driver_temp.
func buildSetTemps(p map[string]any) ([]byte, error) {
	driver, _, err := paramFloat(p, "driver_temp", true)
	if err != nil {
		return nil, err
	}
	passenger, present, err := paramFloat(p, "passenger_temp", false)
	if err != nil {
		return nil, err
	}
	if !present {
		passenger = driver
	}
	return marshalBody(map[string]any{"driver_temp": driver, "passenger_temp": passenger})
}

// buildSetChargeLimit marshals {percent} constrained to Tesla's 50–100 range.
func buildSetChargeLimit(p map[string]any) ([]byte, error) {
	pct, _, err := paramInt(p, "percent", true)
	if err != nil {
		return nil, err
	}
	if pct < 50 || pct > 100 {
		return nil, errInvalidParams("parameter \"percent\" must be between 50 and 100")
	}
	return marshalBody(map[string]any{"percent": pct})
}

// buildActuateTrunk marshals {which_trunk} constrained to front|rear.
func buildActuateTrunk(p map[string]any) ([]byte, error) {
	which, err := paramString(p, "which_trunk")
	if err != nil {
		return nil, err
	}
	if which != "front" && which != "rear" {
		return nil, errInvalidParams("parameter \"which_trunk\" must be \"front\" or \"rear\"")
	}
	return marshalBody(map[string]any{"which_trunk": which})
}

// buildNavigationGPS marshals {lat, lon, order} for navigation_gps_request.
// order is Tesla's 1-based remote-nav order integer: 1 REPLACES the current
// trip (the destination becomes the active route), 2 prepends a stop, 3
// appends a stop. It defaults to 1 (replace-trip) when the caller omits it —
// the right choice for a single lat/long dispatch destination (MYR-176).
// (Source: Tesla Fleet API navigation_gps_request remote-nav order semantics,
// per the Teslemetry python-tesla-fleet-api client — "1 replaces the trip, 2
// prepends a stop, 3 appends a stop".)
func buildNavigationGPS(p map[string]any) ([]byte, error) {
	lat, _, err := paramFloat(p, "lat", true)
	if err != nil {
		return nil, err
	}
	lon, _, err := paramFloat(p, "lon", true)
	if err != nil {
		return nil, err
	}
	if lat < -90 || lat > 90 {
		return nil, errInvalidParams("parameter \"lat\" out of range [-90, 90]")
	}
	if lon < -180 || lon > 180 {
		return nil, errInvalidParams("parameter \"lon\" out of range [-180, 180]")
	}
	order, present, err := paramInt(p, "order", false)
	if err != nil {
		return nil, err
	}
	if !present {
		order = 1
	}
	return marshalBody(map[string]any{"lat": lat, "lon": lon, "order": order})
}

// buildNavigationShare marshals a navigation_request share payload from a
// text destination the CAR's nav geocoder resolves — a street address, a
// "<lat>,<lon>" coordinate pair (what MYR-245 dispatch sends), or a maps URL.
// Tesla's share schema wraps the text in the Android share intent extra.
func buildNavigationShare(p map[string]any) ([]byte, error) {
	value, err := paramString(p, "value")
	if err != nil {
		return nil, err
	}
	return marshalBody(map[string]any{
		"type":         "share_ext_content_raw",
		"locale":       "en-US",
		"timestamp_ms": fmt.Sprintf("%d", time.Now().UnixMilli()),
		"value":        map[string]any{"android.intent.extra.TEXT": value},
	})
}

// buildSeatHeater marshals {seat_position, level} for
// remote_seat_heater_request. seat_position is Tesla's 0-based seat index
// (0 front-left, 1 front-right, then the second/third rows); the pinned proxy
// (v0.4.1) validates 0..8 against its seatPositions table. level is the heater
// setting 0-3 (off/low/med/high, vehicle.Level).
func buildSeatHeater(p map[string]any) ([]byte, error) {
	seat, _, err := paramInt(p, "seat_position", true)
	if err != nil {
		return nil, err
	}
	if seat < 0 || seat > 8 {
		return nil, errInvalidParams("parameter \"seat_position\" must be between 0 and 8")
	}
	level, _, err := paramInt(p, "level", true)
	if err != nil {
		return nil, err
	}
	if level < 0 || level > 3 {
		return nil, errInvalidParams("parameter \"level\" must be between 0 and 3")
	}
	return marshalBody(map[string]any{"seat_position": seat, "level": level})
}

// buildSeatCooler marshals {seat_position, seat_cooler_level} for
// remote_seat_cooler_request. Only the front seats have coolers, and Tesla's
// cooler seat_position enum DIFFERS from the heater's index: 1 front-left,
// 2 front-right (0 is Unknown and the vehicle rejects it).
//
// seat_cooler_level is 1-4 (1 off, 2 low, 3 med, 4 high), NOT 0-3. The HTTP
// value maps 1:1 onto the vehicle's HvacSeatCoolerLevel enum
// (Unknown=0, Off=1, Low=2, Med=3, High=4): the pinned proxy (v0.4.1) does
// vehicle.Level(x-1) in settingForCoolerSeatPosition and SetSeatCooler then
// sends HvacSeatCoolerLevel_E(level+1), so the -1/+1 cancel. (The proxy's
// "The API uses 0-3" comment describes the internal vehicle.Level scale after
// the -1, not this HTTP field.) This is asymmetric with the heater, whose
// level is 0-3 and is passed straight through to vehicle.Level (LevelOff=0).
func buildSeatCooler(p map[string]any) ([]byte, error) {
	seat, _, err := paramInt(p, "seat_position", true)
	if err != nil {
		return nil, err
	}
	if seat != 1 && seat != 2 {
		return nil, errInvalidParams("parameter \"seat_position\" must be 1 (front-left) or 2 (front-right)")
	}
	level, _, err := paramInt(p, "seat_cooler_level", true)
	if err != nil {
		return nil, err
	}
	if level < 1 || level > 4 {
		return nil, errInvalidParams("parameter \"seat_cooler_level\" must be between 1 and 4")
	}
	return marshalBody(map[string]any{"seat_position": seat, "seat_cooler_level": level})
}

// buildAdjustVolume marshals {volume} for adjust_volume. Tesla's media volume
// is a float in [0, 11]; the pinned proxy (v0.4.1) forwards it to SetVolume
// unchanged.
func buildAdjustVolume(p map[string]any) ([]byte, error) {
	vol, _, err := paramFloat(p, "volume", true)
	if err != nil {
		return nil, err
	}
	if vol < 0 || vol > 11 {
		return nil, errInvalidParams("parameter \"volume\" must be between 0 and 11")
	}
	return marshalBody(map[string]any{"volume": vol})
}
