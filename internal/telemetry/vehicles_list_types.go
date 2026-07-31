package telemetry

// The per-row wire shape for GET /api/vehicles. Split out of
// vehicles_list_handler.go (300-line file cap) so the handler file holds the
// request flow and this one holds the projection.

// vehicleSummary is the per-row catalog shape returned by the list
// endpoint. JSON tags mirror the wire schema in rest-api.md §7.0 and
// `VehicleSummary` in specs/rest.openapi.yaml. See also the mask
// allow-list in `internal/mask/tables.go` (vehicleSummaryOwnerFields).
type vehicleSummary struct {
	VehicleID string `json:"vehicleId"`
	Name      string `json:"name"`
	Model     string `json:"model"`
	Year      int    `json:"year"`
	Color     string `json:"color"`
	// LicensePlate (MYR-286) follows the SAME emission convention as its
	// sibling identity field `color`: plain string, NO omitempty, so the
	// key is ALWAYS present and "no plate set" is an empty string rather
	// than a missing key. In BOTH role allow-lists — a rider identifies
	// the car at pickup from this row (contrast VinLast4/`vin`).
	LicensePlate   string `json:"licensePlate"`
	VinLast4       string `json:"vinLast4"`
	Status         string `json:"status"`
	ChargeLevel    int    `json:"chargeLevel"`
	EstimatedRange int    `json:"estimatedRange"`
	LastUpdated    string `json:"lastUpdated"`
	Role           string `json:"role"`
	// HasActiveRide is OPTIONAL on the wire contract but ALWAYS emitted
	// by this server version (true or false) — absence signals a server
	// that predates MYR-233, never "vehicle is free". Consumers treat an
	// absent value as "availability unknown → treat as available".
	HasActiveRide bool `json:"hasActiveRide"`
	// SharePermission is the compatibility projection of the caller's grant
	// over a SHARED vehicle (MYR-184, DERIVED as of MYR-369): `rides` when
	// the grant carries the ride capability, `live` otherwise. Never
	// `live_history` — that tier is retired. Emitted if and only if Role is
	// `viewer`; omitted on owner rows, where it would be meaningless — an
	// owner holds no grant. It is a UI-affordance hint: the server enforces
	// every gate it describes independently, so a client that ignores it
	// cannot escalate.
	SharePermission string `json:"sharePermission,omitempty"`
	// ServiceEstimatedEndAt is when this car's CURRENT SERVICE VISIT is
	// expected to end (MYR-316, contracts v0.17.0) — the same value and the
	// same semantics as VehicleState.serviceEstimatedEndAt. RFC 3339 UTC, or
	// null. Server-computed: Tesla's `service_etc` wins, else the owner-entered
	// §7.16 value, else null. Meaningful ONLY while `status` is `in_service`
	// and null otherwise. ALWAYS emitted (as an explicit null when there is no
	// estimate) so a consumer can tell "no estimate" from a pre-MYR-316 server.
	ServiceEstimatedEndAt *string `json:"serviceEstimatedEndAt"`
	// RideShareEnabled is the owner's ride-sharing switch (MYR-342,
	// contracts v0.20.0). OPTIONAL on the wire contract but ALWAYS emitted by
	// this server version (true or false), with NO omitempty — a `false` that
	// omitempty swallowed would read to a consumer as "absent", which the
	// contract defines as ENABLED, i.e. it would silently un-pause a paused car
	// on every catalog fetch. Absence therefore only ever signals a server
	// predating MYR-342, never a paused vehicle.
	RideShareEnabled bool `json:"rideShareEnabled"`
}

// toMaskMap returns the row as a wire-name-keyed map suitable for
// projection through the role-based mask. Mirrors the pattern in
// vehicle_status_handler.go ToMaskMap.
func (v vehicleSummary) toMaskMap() map[string]any {
	m := v.baseMaskMap()
	// Emitted only on viewer rows (MYR-184). Adding the key unconditionally
	// would put an empty-string tier on every owner row, which a consumer
	// told to treat an absent tier as the LOWEST one would read as "this
	// owner has `live` access to their own car".
	if v.SharePermission != "" {
		m["sharePermission"] = v.SharePermission
	}
	return m
}

// baseMaskMap is the role-independent field set.
func (v vehicleSummary) baseMaskMap() map[string]any {
	return map[string]any{
		"vehicleId":      v.VehicleID,
		"name":           v.Name,
		"model":          v.Model,
		"year":           v.Year,
		"color":          v.Color,
		"licensePlate":   v.LicensePlate,
		"vinLast4":       v.VinLast4,
		"status":         v.Status,
		"chargeLevel":    v.ChargeLevel,
		"estimatedRange": v.EstimatedRange,
		"lastUpdated":    v.LastUpdated,
		"role":           v.Role,
		"hasActiveRide":  v.HasActiveRide,
		// MYR-316 — already resolved (precedence + in-service gate) by
		// buildResponse; this is the emitted value, not a raw column.
		"serviceEstimatedEndAt": v.ServiceEstimatedEndAt,
		// MYR-342 — the owner's switch, emitted raw (nothing to resolve). In
		// the BASE map, not the viewer-only branch below: both role
		// allow-lists carry it.
		"rideShareEnabled": v.RideShareEnabled,
	}
}
