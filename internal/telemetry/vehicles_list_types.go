package telemetry

// The per-row wire SHAPES for GET /api/vehicles. Split out of
// vehicles_list_handler.go (300-line file cap) so the handler file holds the
// request flow and this one holds the shapes.
//
// The function that BUILDS these from a catalog row lives in
// vehicles_list_projection.go — a third file since MYR-507/MYR-515, when two
// more fields pushed the pair over the cap.

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
	// SetupState names the ONE thing still standing between this car and live
	// telemetry, or null when there is nothing to say (MYR-491,
	// contracts v0.24.0) — the SAME object and the SAME semantics as
	// VehicleState.setupState (§7.1), produced by the same derivation over the
	// same inputs.
	//
	// On the CATALOG for the rider-side picker (MYR-437): a car mid-setup must
	// read as "setting up", not be omitted and not be badged "offline", and the
	// picker cannot fetch a snapshot per row to learn which it is.
	//
	// A pointer with NO omitempty: the key is always present, and "nothing to
	// finish" is an explicit null.
	SetupState *SetupState `json:"setupState"`
	// TrimLabel is the display-ready trim of this car — "Plaid", "Performance",
	// "Long Range" (MYR-507, contracts v0.31.0). The SAME value and the SAME
	// column as VehicleState.trimLabel (§7.1, MYR-320): the snapshot does not
	// derive it, it reads it, so both surfaces read one column and there is no
	// second implementation to drift.
	//
	// On the CATALOG because the catalog is the ONLY vehicle-identity surface a
	// RIDER has. Owners get model/trim from the §7.1 snapshot; viewers never
	// fetch one, so a shared car could previously be named only by its colour
	// and a bare make — the "UltraRed" / "Tesla" the field report opened with.
	//
	// A pointer with NO omitempty: the key is always present, and "Tesla has not
	// told us the trim" is an explicit null — never `""`, which a descriptor
	// builder would happily render as an empty fragment between two separators.
	TrimLabel *string `json:"trimLabel"`
	// OwnerFirstName is the FIRST NAME of the person who owns this car
	// (MYR-581, contracts v0.37.0) — resolved by the platform's three-source
	// identity ladder ("User".name → the Apple first-consent name → go_users)
	// and reduced to its first token, the SAME resolution and the SAME
	// reduction behind `RideRequest.requesterName` and
	// `RedeemShareInviteResponse.ownerFirstName`.
	//
	// FIRST NAMES ONLY is the P1 policy for anything delivered to a
	// counterparty, and it is why this is not simply `"User".name`: a
	// counterparty gets "Amruth", never "Amruth Kelkar".
	//
	// On the CATALOG because the catalog is the only surface on which a RIDER
	// meets the person whose car they were granted. Without it the client had no
	// name to render and fell back to the make — which is how an incoming ride
	// card came to read "Tesla wants a ride" about a human being (MYR-532 item
	// 4). Emitted on BOTH roles: an owner's own row naming them is harmless and
	// keeps one projection for all three producers.
	//
	// A pointer with NO omitempty, the trimLabel convention: the key is always
	// present, and "this owner has no resolvable name" is an explicit null —
	// never `""`, which a possessive-descriptor builder would happily render as
	// "'s Model X".
	OwnerFirstName *string `json:"ownerFirstName"`
	// Location is where this car was last known to be (MYR-515,
	// contracts v0.31.0) — the SAME coordinate pair the §7.1 snapshot and the
	// live `vehicle_update` frame carry, from the same encrypted columns.
	//
	// On the CATALOG because the rider picker needs a per-row pickup ETA and has
	// no other input for a row it is not watching: the client holds exactly ONE
	// coordinate (the watched car's, off the single-vehicle socket
	// subscription), and a viewer's per-row /snapshot is 403 by design
	// (MYR-432/449). Without this the picker can only rank the one car it
	// already opened.
	//
	// A NULLABLE OBJECT rather than two nullable scalars, deliberately: the GPS
	// pair is an ATOMIC GROUP (rest-api.md §5.4) and splitting one is its own
	// contract violation. Nesting makes "both or neither" structural instead of
	// conventional — there is no way to express the half-state that two flat
	// fields would permit.
	//
	// NULL means the server holds no fix, and it is the ONLY spelling of that on
	// this surface: the (0, 0) sentinel §7.1 uses is collapsed to null here by
	// newVehicleSummary. A picker must never measure an ETA to the Gulf of
	// Guinea.
	Location *vehicleLocation `json:"location"`
}

// vehicleLocation is the §7.0 position object (MYR-515). Field names follow
// `VehicleState.latitude`/`longitude` and `SavedPlace` — the account/vehicle
// family — rather than RidePlace's `lat`/`lng`. Both spellings exist on the
// platform and this surface belongs to the former.
type vehicleLocation struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// newVehicleLocation builds the wire position, or nil when there is no honest
// one to report (MYR-515).
//
// TWO ways to have no position, collapsed to ONE wire value:
//
//   - the pair is absent — never streamed, or a repo with no encryptor, or a
//     half-pair the store already refused to surface;
//   - the pair is present and is EXACTLY (0, 0) — the no-fix sentinel that
//     vehicle-state-schema.md §2.3 defines for the NOT NULL snapshot columns,
//     where the schema cannot express absence and 0,0 stands in for it.
//
// The second is the one that would actually lie. §7.0 CAN express absence, so
// it does, and the sentinel never reaches a consumer that would have to know to
// re-interpret it. A picker fed 0,0 does not fail — it confidently renders a
// pickup ETA to a point in the Atlantic.
//
// The test is EXACT equality on both axes, not a tolerance: a real fix that
// rounds to within metres of Null Island is not a thing the sentinel needs to
// protect against, and widening the test would start discarding genuine
// coordinates off the West African coast.
func newVehicleLocation(lat, lng *float64) *vehicleLocation {
	if lat == nil || lng == nil {
		return nil
	}
	if *lat == 0 && *lng == 0 {
		return nil
	}
	return &vehicleLocation{Latitude: *lat, Longitude: *lng}
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
		// MYR-491 — already derived by newVehicleSummary; this is the emitted
		// value. In the BASE map for the same reason as the sibling above: both
		// role allow-lists carry it, and the VIEWER is the party a "setting up"
		// row is most useful to (MYR-437's picker).
		"setupState": setupStateWire(v.SetupState),
		// MYR-507 — the display-safe trim, emitted raw (nothing to resolve). In
		// the BASE map, like its identity siblings `model` / `year` / `color`:
		// both role allow-lists carry it, and the VIEWER is the party who cannot
		// name the car any other way.
		// derefOrNil (vehicle_status_handler.go) is the SAME helper the snapshot
		// uses for this SAME field, so an unset trim maps to an untyped nil on
		// both surfaces rather than a typed (*string)(nil) on one of them.
		"trimLabel": derefOrNil(v.TrimLabel),
		// MYR-581 — the owner's first name, emitted raw (the ladder and the
		// first-token reduction both ran in the store). In the BASE map, not the
		// viewer branch below: BOTH role allow-lists carry it, and the VIEWER is
		// the party the field exists for — see internal/mask/tables.go for why a
		// first name is the right disclosure and a full name is not.
		// derefOrNil for the same reason as `trimLabel` above: an unset name
		// must map to an untyped nil rather than a typed (*string)(nil), which
		// marshals to `null` but is not `== nil` to a mask predicate or a test.
		"ownerFirstName": derefOrNil(v.OwnerFirstName),
		// MYR-515 — already resolved (atomic pair + sentinel collapse) by
		// newVehicleSummary; this is the emitted value, not a raw column pair.
		// In the BASE map: BOTH role allow-lists carry it, and the viewer is the
		// party the field exists for (see internal/mask/tables.go for why that
		// discloses nothing a viewer cannot already stream).
		"location": locationWire(v.Location),
	}
}

// locationWire flattens the position object for the mask map, or an untyped nil.
//
// Returning the *vehicleLocation directly would put a typed (*vehicleLocation)(nil)
// into the map — which marshals to `null` but is not `== nil` to any test or
// mask predicate reading the map. The same trap setupStateWire exists to avoid
// for its own nested type.
func locationWire(l *vehicleLocation) any {
	if l == nil {
		return nil
	}
	return map[string]any{
		"latitude":  l.Latitude,
		"longitude": l.Longitude,
	}
}
