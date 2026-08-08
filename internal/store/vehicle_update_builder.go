// VehicleUpdate → SQL helpers split out of queries.go to keep both
// files under the 300-line cap. The dynamic UPDATE builder is the only
// non-trivial logic in the store SQL layer; everything else is a
// constant query string.

package store

import (
	"fmt"
	"strings"
)

// updateColumn pairs a PostgreSQL column name with the value to set. A nil
// value signals that the field was not present in this telemetry event.
type updateColumn struct {
	col  string
	val  any    // nil when the field pointer is nil
	cast string // optional PostgreSQL type cast (e.g. "::jsonb")
}

// updateColumns returns the list of column/value pairs for a VehicleUpdate.
// Values are dereferenced so callers can check for nil uniformly.
//
// MYR-433: the seven location columns ("latitude", "longitude",
// "destinationLatitude", "destinationLongitude", "originLatitude",
// "originLongitude", "navRouteCoordinates") are deliberately ABSENT from
// this list. Coordinates are written only as ciphertext, by
// appendGPSShadowSets and appendNavRouteShadowSet. Re-adding any of them
// here re-opens the exact hole MYR-433 closed: it would make a user's
// live position readable to anyone with a database connection.
//
// MYR-447 removed four more for the same reason: "locationName",
// "locationAddress", "destinationName" and "destinationAddress". Those
// are the reverse-geocoded rendering of the coordinates above — a street
// address and a place name — and leaving them here meant the sealed
// coordinate sat next to a plaintext column saying where it was in
// English. They are written only as ciphertext, by appendLabelShadowSets.
//
// The plaintext columns are left untouched rather than cleared because
// this server no longer owns them — `cmd/purge-plaintext-columns` is what
// scrubs their pre-MYR-433/447 residue, once it has verified the
// ciphertext sibling decrypts.
func updateColumns(u VehicleUpdate) []updateColumn {
	return []updateColumn{
		{"speed", derefInt(u.Speed), ""},
		{"chargeLevel", derefInt(u.ChargeLevel), ""},
		{"estimatedRange", derefInt(u.EstimatedRange), ""},
		{"chargeState", derefString(u.ChargeState), ""},
		{"timeToFull", derefFloat(u.TimeToFull), ""},
		{"gearPosition", derefString(u.GearPosition), ""},
		{"heading", derefInt(u.Heading), ""},
		{"interiorTemp", derefInt(u.InteriorTemp), ""},
		{"exteriorTemp", derefInt(u.ExteriorTemp), ""},
		{"odometerMiles", derefInt(u.OdometerMiles), ""},
		{"fsdMilesSinceReset", derefFloat(u.FsdMilesSinceReset), ""},
		{"etaMinutes", derefInt(u.EtaMinutes), ""},
		{"tripDistanceRemaining", derefFloat(u.TripDistRemaining), ""},
	}
}

// deref helpers convert typed pointers to any, returning nil when the
// pointer is nil so the caller can skip the column.
func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// buildTelemetryUpdate constructs a dynamic UPDATE statement for
// VehicleUpdate, including only columns whose values are non-nil.
// Returns the query string, the argument slice, and whether any fields
// were set. The caller should skip the UPDATE when ok is false.
//
// encShadows holds pre-computed *Enc ciphertexts keyed by *Enc column
// name (e.g. "latitudeEnc", "navRouteCoordinatesEnc"). Atomic-pair
// invariant for GPS pairs: callers MUST only insert keys in
// pair-complete pairs (both halves or neither) — see
// buildEncryptedGPSPair. nil/empty map disables the dual-write
// entirely (useful in tests that don't inject an Encryptor).
func buildTelemetryUpdate(vin string, u VehicleUpdate, encShadows map[string]string) (query string, args []any, ok bool) {
	clearSet := buildClearSet(u.ClearFields)

	var setClauses []string
	argIdx := 1

	setClauses, args, argIdx = appendPlaintextSets(setClauses, args, argIdx, u, clearSet)
	setClauses, args, argIdx = appendDerivedStatusSet(setClauses, args, argIdx, u, clearSet)
	setClauses, args, argIdx = appendGPSShadowSets(setClauses, args, argIdx, encShadows, clearSet)
	setClauses, args, argIdx = appendNavRouteShadowSet(setClauses, args, argIdx, encShadows, clearSet)
	setClauses, args, argIdx = appendLabelShadowSets(setClauses, args, argIdx, encShadows, clearSet)
	setClauses = appendClearFieldSets(setClauses, u.ClearFields)

	if len(setClauses) == 0 {
		return "", nil, false
	}

	// Always update lastUpdated.
	setClauses = append(setClauses, fmt.Sprintf(`"lastUpdated" = $%d`, argIdx))
	args = append(args, u.LastUpdated)
	argIdx++

	// VIN is the final parameter for the WHERE clause.
	args = append(args, vin)
	query = fmt.Sprintf(`UPDATE "Vehicle" SET %s WHERE "vin" = $%d`,
		strings.Join(setClauses, ", "), argIdx)
	return query, args, true
}

// buildClearSet flips the ClearFields slice into a set so the inner
// loops can short-circuit a column being explicitly cleared.
func buildClearSet(clearFields []string) map[string]bool {
	clearSet := make(map[string]bool, len(clearFields))
	for _, col := range clearFields {
		clearSet[col] = true
	}
	return clearSet
}

// appendPlaintextSets walks every non-nil VehicleUpdate field and
// appends the corresponding `<col> = $N[::cast]` clause + argument.
// Columns being explicitly cleared are skipped (handled later by
// appendClearFieldSets). Named returns mirror the input names so the
// caller's threading reads cleanly.
func appendPlaintextSets(
	setClauses []string, args []any, argIdx int,
	u VehicleUpdate, clearSet map[string]bool,
) (clauses []string, outArgs []any, nextIdx int) {
	for _, col := range updateColumns(u) {
		if col.val == nil || clearSet[col.col] {
			continue
		}
		// %q produces Go double-quoted strings which match PostgreSQL's
		// double-quoted identifier syntax. Column names are constants.
		setClauses = append(setClauses, fmt.Sprintf("%q = $%d%s", col.col, argIdx, col.cast))
		args = append(args, col.val)
		argIdx++
	}
	return setClauses, args, argIdx
}

// appendDerivedStatusSet folds "status" into the SAME statement that writes
// the motion columns it is derived from (MYR-454, MYR-61).
//
// Why this exists. Before MYR-454 the column had exactly two writers —
// `drive.ended` → 'parked' and the ServiceStatusMonitor → 'in_service' /
// 'parked' — and NEITHER of them, nor this telemetry flush, ever wrote
// 'driving'. `drive.started` wrote no status at all, the asymmetric half of
// a pair. So the persisted column could only ever hold 'offline' (the schema
// default), 'parked' or 'in_service', while `driving` existed solely as a
// live re-derivation in the WebSocket broadcaster (ws.deriveVehicleStatus).
// Every surface that reads the column — the REST snapshot, the WS
// on-connect snapshot, the vehicles list that backs the map sheet — served
// 'parked' for the entire duration of a real drive. That is the external-beta
// report: a car mid-drive, drive row open, `speed`/`gearPosition` freshly
// written to its own row, and `status` still saying 'parked' beside them.
//
// The fix is to stop treating status as a thing someone remembers to write
// and make it a function of the row's own motion columns, evaluated in SQL so
// it is ATOMIC with the gear/speed write it follows. There is no window in
// which the row can disagree with itself, and no second writer to race: the
// derivation IS the column.
//
// Bare column references on the right-hand side of a SET read the OLD row, so
// COALESCE($new, "col") means "this frame's value if it carried one, else the
// last one we stored". That is what lets a speed-only frame — the common case,
// since Tesla emits Gear on CHANGE only and a car in D sends no further gear
// frames for the rest of the drive — still resolve against the gear the car
// was last known to be in.
//
// Precedence mirrors ws.deriveVehicleStatus (vehicle-state-schema.md §2.4)
// so the snapshot and the live wire cannot disagree: driving (gear D/R, or
// speed > 0) always wins, then in_service, else parked.
//
// Two deliberate choices in the CASE:
//
//   - 'in_service' and 'charging' are PRESERVED rather than overwritten when
//     the car is stationary. This server owns neither: 'in_service' belongs to
//     the ServiceStatusMonitor and 'charging' is written by no Go path at all
//     (the Vehicle table is Prisma-owned and shared with the Next.js app).
//     Folding them down to 'parked' on every 5s flush would stomp another
//     writer's value at a cadence it could never win against.
//   - Everything else stationary becomes 'parked' — INCLUDING 'offline'. A row
//     whose status is 'offline' while its own gear/speed columns are being
//     updated by a live stream is self-contradictory: 'offline' is the schema
//     default that owner-vehicle provisioning never overwrites, and it is what
//     leaves a happily-streaming car permanently ineligible for dispatch. A
//     frame arriving is proof the car is not offline, so ingest heals it.
//
// Known limitation, deliberately not papered over: a car that is in_service
// and then drives becomes 'driving', and on parking resolves to 'parked'
// rather than back to 'in_service'. The live wire does return to in_service,
// because the broadcaster caches the ServiceMode bool separately — but there
// is no persisted serviceMode column for this CASE to consult, and inventing
// one is out of scope here. The MYR-320 periodic in-service pass re-asserts
// the value on its next sweep.
func appendDerivedStatusSet(
	setClauses []string, args []any, argIdx int,
	u VehicleUpdate, clearSet map[string]bool,
) (clauses []string, outArgs []any, nextIdx int) {
	gear := derefString(u.GearPosition)
	if clearSet["gearPosition"] {
		gear = nil
	}
	speed := derefInt(u.Speed)
	if clearSet["speed"] {
		speed = nil
	}

	// No motion signal in this frame means nothing to re-derive from. A
	// climate-only or media-only frame must leave status exactly as it was.
	if gear == nil && speed == nil {
		return setClauses, args, argIdx
	}

	setClauses = append(setClauses, fmt.Sprintf(
		`"status" = CASE`+
			` WHEN COALESCE($%d::text, "gearPosition") IN ('D', 'R') THEN 'driving'`+
			` WHEN COALESCE($%d::int, "speed") > 0 THEN 'driving'`+
			` WHEN "status" = 'in_service' THEN 'in_service'`+
			` WHEN "status" = 'charging' THEN 'charging'`+
			` ELSE 'parked'`+
			` END::"VehicleStatus"`,
		argIdx, argIdx+1))
	args = append(args, gear, speed)
	return setClauses, args, argIdx + 2
}

// appendGPSShadowSets emits the MYR-63 GPS *Enc shadows in canonical
// pair order. Half-pair entries are skipped defensively — the caller's
// atomic-pair guard should reject them, but a half-pair UPDATE would
// trip the read-side fallback, so we filter again.
func appendGPSShadowSets(
	setClauses []string, args []any, argIdx int,
	encShadows map[string]string, clearSet map[string]bool,
) (clauses []string, outArgs []any, nextIdx int) {
	for _, p := range gpsPairs {
		latEncCol := p.lat + "Enc"
		lngEncCol := p.lng + "Enc"
		latCT, hasLat := encShadows[latEncCol]
		lngCT, hasLng := encShadows[lngEncCol]
		if !hasLat || !hasLng {
			continue
		}
		if clearSet[p.lat] || clearSet[p.lng] {
			continue
		}
		setClauses = append(setClauses, fmt.Sprintf("%q = $%d", latEncCol, argIdx))
		args = append(args, latCT)
		argIdx++
		setClauses = append(setClauses, fmt.Sprintf("%q = $%d", lngEncCol, argIdx))
		args = append(args, lngCT)
		argIdx++
	}
	return setClauses, args, argIdx
}

// appendNavRouteShadowSet emits the MYR-64 nav-route blob shadow when
// the encrypted ciphertext is present and the plaintext column isn't
// being concurrently cleared.
func appendNavRouteShadowSet(
	setClauses []string, args []any, argIdx int,
	encShadows map[string]string, clearSet map[string]bool,
) (clauses []string, outArgs []any, nextIdx int) {
	navCT, ok := encShadows["navRouteCoordinatesEnc"]
	if !ok || clearSet["navRouteCoordinates"] {
		return setClauses, args, argIdx
	}
	setClauses = append(setClauses, fmt.Sprintf("%q = $%d", "navRouteCoordinatesEnc", argIdx))
	args = append(args, navCT)
	argIdx++
	return setClauses, args, argIdx
}

// appendLabelShadowSets emits the MYR-447 location-label `*Enc` shadows.
// This is the replacement for the four plaintext label columns that
// updateColumns used to carry; a label reaches the database only through
// here.
//
// Two behaviours worth naming:
//
//   - An EMPTY ciphertext means the update carried an empty label (the
//     geocoder returned nothing). That is written as an explicit
//     `= NULL`, not as an empty-string ciphertext, so "no label" has one
//     representation in the column and queryDriveMissingAddresses-style
//     `IS NULL` predicates stay meaningful. It still has to be WRITTEN
//     rather than skipped: the field was present in the update, so a
//     previously-known address must be cleared, not left stale.
//   - A column being explicitly cleared (ClearFields) is skipped here and
//     handled by appendClearFieldSets, which NULLs the same `*Enc`
//     column. Emitting both would produce a duplicate SET.
func appendLabelShadowSets(
	setClauses []string, args []any, argIdx int,
	encShadows map[string]string, clearSet map[string]bool,
) (clauses []string, outArgs []any, nextIdx int) {
	for _, plain := range vehicleLabelColumns {
		encCol := plain + "Enc"
		ct, ok := encShadows[encCol]
		if !ok || clearSet[plain] {
			continue
		}
		if ct == "" {
			// An EMPTY label is a clear, and must be written like one.
			//
			// This is not a rare path. A reverse geocode that resolves a
			// street address but no place name writes LocationName="" with
			// LocationAddr set (writer_location_address.go), and the
			// destination geocode does the same for DestinationAddress —
			// so a car habitually parked somewhere address-only re-enters
			// this branch on every geocode.
			//
			// NULLing the ciphertext alone would leave plaintext-present +
			// ciphertext-NULL, which plaintextpurge reads as
			// verdictUnsealed and refuses to touch — permanently, and it
			// would recur after every purge. Its printed advice ("run the
			// backfill first") then re-seals the STALE plaintext, putting a
			// label the server had just cleared back onto the live read
			// path. Same failure as the nav-cancel case; same fix.
			setClauses = appendLabelClear(setClauses, plain, encCol)
			continue
		}
		setClauses = append(setClauses, fmt.Sprintf("%q = $%d", encCol, argIdx))
		args = append(args, ct)
		argIdx++
	}
	return setClauses, args, argIdx
}

// appendClearFieldSets emits an explicit `SET NULL` for every
// ClearFields entry, so a "navigation cancelled" event actually empties
// the row.
//
// MYR-433 split the behaviour by column family. ClearFields is keyed on
// PLAINTEXT column names (that is the vocabulary navFieldColumns and
// applyRouteLine speak), but for the location columns this server no
// longer owns the plaintext copy — it owns the ciphertext. So:
//
//   - A location column (one with an *Enc sibling) clears the *Enc
//     column ONLY. Clearing the plaintext column too would be a write to
//     a column we otherwise never touch, and for "latitude"/"longitude"
//     it would fail outright — they are NOT NULL on the Prisma schema.
//   - Every other column (destinationName, etaMinutes, …) clears as
//     before; those hold no coordinates and this server still owns them.
//
// The invariant that matters is unchanged: after a cancel, no stale
// route or destination survives in the column the read path consults.
func appendClearFieldSets(setClauses, clearFields []string) []string {
	for _, col := range clearFields {
		if encCol, isLocation := plaintextToEncColumn[col]; isLocation {
			setClauses = append(setClauses, fmt.Sprintf("%q = NULL", encCol))
			// MYR-447: for the LABEL columns, also scrub the retired
			// plaintext. Clearing the ciphertext alone would leave the row
			// in the one state the purge cannot resolve — plaintext
			// present, ciphertext NULL — which plaintextpurge reads as
			// "never sealed" (verdictUnsealed) and deliberately refuses to
			// touch, because for a legacy row the plaintext really is the
			// only copy. A nav-cancel would therefore park a readable
			// place name in the table permanently and hold
			// `remaining` above zero forever, blocking the column drop.
			//
			// Worse than the counter: the tool's own advice on an unsealed
			// row is "run the backfill first", and the backfill's
			// predicate (plaintext <> '' AND enc IS NULL) matches exactly
			// this row — it would re-seal a destination the driver
			// CANCELLED, resurrecting it onto the snapshot and the WS
			// replay beside a NULL destination coordinate, i.e. a broken
			// navigation atomic group built out of a stale P1 place name.
			//
			// Scrubbing both is what "cleared" has to mean once the
			// plaintext is retired rather than dropped. The value matches
			// the column's nullability, same as the purge's ScrubSQL.
			if _, isLabel := labelPlaintextScrubValue[col]; isLabel {
				setClauses = appendLabelPlaintextScrub(setClauses, col)
			}
			continue
		}
		setClauses = append(setClauses, fmt.Sprintf("%q = NULL", col))
	}
	return setClauses
}

// labelPlaintextScrubValue gives the "no data here" literal for each
// retired plaintext LABEL column, tracking its nullability in the
// react-frontend Prisma schema. It deliberately mirrors the ScrubSQL
// column of plaintextpurge's target table — the two must agree, or a
// cleared row and a purged row would end up in different states and only
// one of them would satisfy the purge's HasData predicate.
//
// The GPS columns are absent on purpose. They have the same latent shape
// (a cancelled destination leaves destinationLatitude non-zero with a NULL
// *Enc sibling) but that predates MYR-447, their scrub value is `0` rather
// than a string, and changing MYR-433's cancel behaviour is not this
// issue's to make. Tracked as a follow-up.
// appendLabelClear writes the two SET clauses that "this label is now
// empty" has to mean while the plaintext column is retired-but-not-dropped:
// NULL the ciphertext this server owns, and scrub the plaintext it no
// longer writes.
//
// Both callers — an explicit ClearFields entry and an empty-string label
// arriving through the normal write path — MUST produce the identical pair.
// They are the same event as far as the database is concerned, and the one
// state neither may leave behind is plaintext-present + ciphertext-NULL,
// which the purge cannot resolve and the backfill would undo.
func appendLabelClear(setClauses []string, plaintextCol, encCol string) []string {
	setClauses = append(setClauses, fmt.Sprintf("%q = NULL", encCol))
	return appendLabelPlaintextScrub(setClauses, plaintextCol)
}

// appendLabelPlaintextScrub emits the "no data here" write for a retired
// plaintext LABEL column, using the literal that matches its nullability.
func appendLabelPlaintextScrub(setClauses []string, plaintextCol string) []string {
	scrub, ok := labelPlaintextScrubValue[plaintextCol]
	if !ok {
		return setClauses
	}
	return append(setClauses, fmt.Sprintf("%q = %s", plaintextCol, scrub))
}

var labelPlaintextScrubValue = map[string]string{
	"locationName":       `''`,
	"locationAddress":    `''`,
	"destinationName":    "NULL",
	"destinationAddress": "NULL",
}

// plaintextToEncColumn maps each retired plaintext location column to the
// *Enc shadow that replaced it. Membership in this map is what marks a
// column as "ciphertext-owned" for appendClearFieldSets and
// appendGPSShadowSets.
//
// MYR-447 added the four label columns. That entry is load-bearing rather
// than cosmetic: navFieldColumns (field_mapper.go) puts
// "destinationName" and "destinationAddress" in ClearFields when the car
// reports navigation cancelled, and without a mapping here the cancel
// would NULL the retired plaintext column while the ciphertext column
// kept serving the address of the destination the user just abandoned.
var plaintextToEncColumn = map[string]string{
	"latitude":             "latitudeEnc",
	"longitude":            "longitudeEnc",
	"destinationLatitude":  "destinationLatitudeEnc",
	"destinationLongitude": "destinationLongitudeEnc",
	"originLatitude":       "originLatitudeEnc",
	"originLongitude":      "originLongitudeEnc",
	"navRouteCoordinates":  "navRouteCoordinatesEnc",
	"locationName":         "locationNameEnc",
	"locationAddress":      "locationAddressEnc",
	"destinationName":      "destinationNameEnc",
	"destinationAddress":   "destinationAddressEnc",
}
