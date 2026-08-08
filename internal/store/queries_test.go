package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestBuildTelemetryUpdate_ClearFields pins how a "navigation cancelled"
// event turns into SQL.
//
// ClearFields is still keyed on PLAINTEXT column names — that is the
// vocabulary the nav-field tables and the writer speak — but MYR-433
// changed which column the clause lands on, and MYR-447 split the answer
// three ways. The cases below pin all three:
//
//   - COORDINATE columns clear the *Enc sibling ONLY. `"latitude"` and
//     `"longitude"` are NOT NULL on the Prisma schema, so a
//     `"latitude" = NULL` clause would fail the whole UPDATE and drop the
//     telemetry tick.
//   - LABEL columns (MYR-447) clear the *Enc sibling AND scrub the retired
//     plaintext. Clearing only the ciphertext leaves the row in the one
//     state plaintextpurge cannot resolve — plaintext present, ciphertext
//     NULL — which it reads as "never sealed" and refuses to touch, and
//     which the label backfill would then RE-SEAL, resurrecting a
//     destination the driver had cancelled onto the live read path.
//   - NON-LOCATION columns (etaMinutes, …) hold no location and clear in
//     place, as they always did.
//
// The scrub value tracks nullability, matching plaintextpurge's ScrubSQL:
// NULL for the nullable destination labels, the empty string for the NOT
// NULL ones.
func TestBuildTelemetryUpdate_ClearFields(t *testing.T) {
	tests := []struct {
		name         string
		vin          string
		update       VehicleUpdate
		wantOK       bool
		wantNulls    []string // columns expected in "col" = NULL clauses
		wantNoNulls  []string // columns that must NOT be NULLed
		wantNoParams bool     // true if no parameterized SET clauses expected (only NULLs + lastUpdated)
	}{
		{
			name: "ClearFields only produces NULL clauses",
			vin:  "5YJ3E1EA1NF000001",
			update: VehicleUpdate{
				ClearFields: []string{"destinationName", "etaMinutes"},
				LastUpdated: time.Now(),
			},
			wantOK: true,
			// MYR-447: destinationName is a label column now, so the clear
			// lands on its ciphertext sibling — AND scrubs the retired
			// plaintext, which is the half that is easy to get wrong.
			// Clearing only the ciphertext would park a readable place name
			// in a state the purge reads as "never sealed" and refuses to
			// touch, and that the backfill would then RE-SEAL, resurrecting
			// a destination the driver cancelled. destinationName is
			// nullable, so its scrub value is NULL.
			wantNulls:    []string{"destinationNameEnc", "destinationName", "etaMinutes"},
			wantNoParams: true,
		},
		{
			name: "ClearFields mixed with regular fields",
			vin:  "5YJ3E1EA1NF000001",
			update: VehicleUpdate{
				Speed:       intPtr(65),
				ClearFields: []string{"originLatitude", "originLongitude"},
				LastUpdated: time.Now(),
			},
			wantOK:      true,
			wantNulls:   []string{"originLatitudeEnc", "originLongitudeEnc"},
			wantNoNulls: []string{"originLatitude", "originLongitude"},
		},
		{
			name: "no ClearFields and no regular fields returns not ok",
			vin:  "5YJ3E1EA1NF000001",
			update: VehicleUpdate{
				LastUpdated: time.Now(),
			},
			wantOK: false,
		},
		{
			name: "all nav columns cleared at once",
			vin:  "5YJ3E1EA1NF000001",
			update: VehicleUpdate{
				ClearFields: []string{
					"destinationName",
					"etaMinutes",
					"tripDistanceRemaining",
					"destinationLatitude",
					"destinationLongitude",
					"originLatitude",
					"originLongitude",
				},
				LastUpdated: time.Now(),
			},
			wantOK: true,
			wantNulls: []string{
				// Non-location columns clear in place…
				"etaMinutes",
				"tripDistanceRemaining",
				// …location columns clear their ciphertext sibling. MYR-447
				// put destinationName in that family: the place name of a
				// cancelled destination is location data too.
				"destinationNameEnc",
				// …and a LABEL column additionally scrubs its retired
				// plaintext (MYR-447), unlike a coordinate column, whose
				// plaintext is NOT NULL and whose scrub value is 0 rather
				// than NULL. That asymmetry is why only the labels appear
				// in both lists.
				"destinationName",
				"destinationLatitudeEnc",
				"destinationLongitudeEnc",
				"originLatitudeEnc",
				"originLongitudeEnc",
			},
			wantNoNulls: []string{
				"destinationLatitude",
				"destinationLongitude",
				"originLatitude",
				"originLongitude",
			},
		},
	}

	runClearFieldCases(t, tests)
}

// TestBuildTelemetryUpdate_EmptyLabelClearsBothColumns covers the OTHER way a
// label becomes empty, which is not ClearFields and is far more common.
//
// A reverse geocode that resolves a street address but no place name writes
// LocationName="" alongside a populated LocationAddr; the destination
// geocode does the same for DestinationAddress. Those arrive as a normal
// update with a non-nil, empty string, so they reach appendLabelShadowSets
// rather than appendClearFieldSets — a completely separate branch, which is
// why this needs its own test and why the bug survived the first one.
//
// The empty label must clear BOTH columns, exactly as ClearFields does.
// Emitting only `"…Enc" = NULL` would leave plaintext-present +
// ciphertext-NULL, which plaintextpurge classifies as verdictUnsealed and
// refuses to touch — permanently, and recurring after every purge, since a
// car parked somewhere address-only re-enters this branch on every geocode.
func TestBuildTelemetryUpdate_EmptyLabelClearsBothColumns(t *testing.T) {
	empty := ""
	// Every label column, with the scrub literal its nullability demands.
	cases := []struct {
		col       string
		update    VehicleUpdate
		wantScrub string
	}{
		{"locationName", VehicleUpdate{LocationName: &empty}, `''`},
		{"locationAddress", VehicleUpdate{LocationAddr: &empty}, `''`},
		{"destinationName", VehicleUpdate{DestinationName: &empty}, "NULL"},
		{"destinationAddress", VehicleUpdate{DestinationAddress: &empty}, "NULL"},
	}

	for _, tc := range cases {
		t.Run(tc.col, func(t *testing.T) {
			encCol := tc.col + "Enc"
			u := tc.update
			u.LastUpdated = time.Now()

			// labelToEncString maps "" to "" (the absent sentinel), which is
			// exactly what addLabelShadows puts in the map for an empty
			// label. Reproduce that rather than guessing at it.
			query, _, ok := buildTelemetryUpdate("5YJ3E1EA1NF000001", u,
				map[string]string{encCol: ""})
			if !ok {
				t.Fatal("expected an update to be built")
			}

			if want := `"` + encCol + `" = NULL`; !strings.Contains(query, want) {
				t.Errorf("missing %q in:\n%s", want, query)
			}
			// The half that was missing: the retired plaintext must be
			// scrubbed too, or the row is stranded unsealed forever.
			if want := `"` + tc.col + `" = ` + tc.wantScrub; !strings.Contains(query, want) {
				t.Errorf("empty label did not scrub the retired plaintext: missing %q in:\n%s\n"+
					"plaintext-present + ciphertext-NULL is the one state the purge cannot "+
					"resolve, and the backfill would re-seal the stale value onto the read path",
					want, query)
			}
		})
	}
}

func runClearFieldCases(t *testing.T, tests []struct {
	name         string
	vin          string
	update       VehicleUpdate
	wantOK       bool
	wantNulls    []string
	wantNoNulls  []string
	wantNoParams bool
},
) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, args, ok := buildTelemetryUpdate(tt.vin, tt.update, nil)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}

			// Verify NULL clauses appear in the query.
			for _, col := range tt.wantNulls {
				nullClause := `"` + col + `" = NULL`
				if !strings.Contains(query, nullClause) {
					t.Errorf("query missing NULL clause for %q:\n%s", col, query)
				}
			}
			// …and that the retired plaintext columns are not touched.
			for _, col := range tt.wantNoNulls {
				nullClause := `"` + col + `" = NULL`
				if strings.Contains(query, nullClause) {
					t.Errorf("query NULLs retired plaintext column %q; the clear belongs on %qEnc:\n%s",
						col, col, query)
				}
			}

			// NULL columns should NOT appear as parameterized args.
			// The args should contain: regular field values + lastUpdated + VIN.
			if tt.wantNoParams {
				// Only lastUpdated + VIN should be in args.
				if len(args) != 2 {
					t.Errorf("args = %d values, want 2 (lastUpdated + VIN); args=%v", len(args), args)
				}
			}

			// VIN should always be the last arg (for WHERE clause).
			if len(args) > 0 && args[len(args)-1] != tt.vin {
				t.Errorf("last arg = %v, want VIN %q", args[len(args)-1], tt.vin)
			}

			// Verify the query has a WHERE clause with the correct VIN parameter.
			if !strings.Contains(query, `WHERE "vin"`) {
				t.Errorf("query missing WHERE vin clause:\n%s", query)
			}
		})
	}
}

func TestBuildTelemetryUpdate_NewNavFields(t *testing.T) {
	tests := []struct {
		name    string
		update  VehicleUpdate
		wantCol string
	}{
		{
			name:    "etaMinutes included in SET clause",
			update:  VehicleUpdate{EtaMinutes: intPtr(15), LastUpdated: time.Now()},
			wantCol: "etaMinutes",
		},
		{
			name:    "tripDistanceRemaining included in SET clause",
			update:  VehicleUpdate{TripDistRemaining: floatPtr(8.3), LastUpdated: time.Now()},
			wantCol: "tripDistanceRemaining",
		},
		{
			name:    "chargeState included in SET clause",
			update:  VehicleUpdate{ChargeState: strPtr("Charging"), LastUpdated: time.Now()},
			wantCol: "chargeState",
		},
		{
			name:    "timeToFull included in SET clause",
			update:  VehicleUpdate{TimeToFull: floatPtr(1.0667), LastUpdated: time.Now()},
			wantCol: "timeToFull",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, _, ok := buildTelemetryUpdate("TEST_VIN", tt.update, nil)
			if !ok {
				t.Fatal("expected ok=true")
			}
			if !strings.Contains(query, `"`+tt.wantCol+`"`) {
				t.Errorf("query missing column %q:\n%s", tt.wantCol, query)
			}
		})
	}
}

// TestBuildTelemetryUpdate_NavRouteCoordinates pins where a nav route
// lands in SQL after MYR-433: the ciphertext the caller pre-computed goes
// into navRouteCoordinatesEnc as a plain TEXT parameter, and the jsonb
// column the route used to be written to never appears in the statement.
//
// The route is a polyline of where the driver is about to go, so the
// absence of the plaintext column is the assertion carrying the security
// property. The dropped ::jsonb cast follows from that — ciphertext is
// opaque TEXT, and Postgres could not parse it as JSON anyway.
func TestBuildTelemetryUpdate_NavRouteCoordinates(t *testing.T) {
	coords := json.RawMessage(`[[-96.77,32.87],[-96.78,32.88]]`)
	update := VehicleUpdate{
		NavRouteCoordinates: &coords,
		LastUpdated:         time.Now(),
	}
	const navCT = "v1:ciphertext-blob"

	query, args, ok := buildTelemetryUpdate("TEST_VIN", update,
		map[string]string{"navRouteCoordinatesEnc": navCT})
	if !ok {
		t.Fatal("expected ok=true")
	}

	if !strings.Contains(query, `"navRouteCoordinatesEnc"`) {
		t.Errorf("query missing navRouteCoordinatesEnc column:\n%s", query)
	}
	if strings.Contains(query, `"navRouteCoordinates"`) {
		t.Errorf("query writes the retired plaintext navRouteCoordinates column:\n%s", query)
	}
	if strings.Contains(query, "::jsonb") {
		t.Errorf("query casts ciphertext to jsonb:\n%s", query)
	}

	// args should be: navRouteCoordinatesEnc ciphertext, lastUpdated, VIN.
	if len(args) != 3 {
		t.Fatalf("args = %d values, want 3", len(args))
	}
	if args[0] != navCT {
		t.Errorf("first arg = %v, want the ciphertext %q", args[0], navCT)
	}
	if args[len(args)-1] != "TEST_VIN" {
		t.Errorf("last arg = %v, want TEST_VIN", args[len(args)-1])
	}
}

// TestBuildTelemetryUpdate_NavRouteWithoutShadowWritesNothing is the
// companion guard: with no ciphertext supplied (no Encryptor wired), a
// NavRouteCoordinates update produces NO statement at all rather than
// quietly falling back to the plaintext column. Losing the route is the
// intended outcome — writing it readable is what MYR-433 forbids.
func TestBuildTelemetryUpdate_NavRouteWithoutShadowWritesNothing(t *testing.T) {
	coords := json.RawMessage(`[[-96.77,32.87]]`)
	query, _, ok := buildTelemetryUpdate("TEST_VIN", VehicleUpdate{
		NavRouteCoordinates: &coords,
		LastUpdated:         time.Now(),
	}, nil)
	if ok {
		t.Errorf("ok = true, want false; query wrote the route without an encryptor:\n%s", query)
	}
}

// TestBuildTelemetryUpdate_NavRouteCoordinatesClear verifies a
// navigation-cancelled clear NULLs the ciphertext column — the only copy
// the read path consults. Clearing the plaintext column instead would
// leave the stale encrypted route live and the app would keep drawing a
// route the driver already ended.
func TestBuildTelemetryUpdate_NavRouteCoordinatesClear(t *testing.T) {
	update := VehicleUpdate{
		ClearFields: []string{"navRouteCoordinates"},
		LastUpdated: time.Now(),
	}

	query, _, ok := buildTelemetryUpdate("TEST_VIN", update, nil)
	if !ok {
		t.Fatal("expected ok=true for ClearFields-only update")
	}

	if !strings.Contains(query, `"navRouteCoordinatesEnc" = NULL`) {
		t.Errorf("query missing NULL clause for navRouteCoordinatesEnc:\n%s", query)
	}
	if strings.Contains(query, `"navRouteCoordinates" = NULL`) {
		t.Errorf("query NULLs the retired plaintext column:\n%s", query)
	}
}
