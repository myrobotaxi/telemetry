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
// changed which column the clause lands on. For the seven location
// columns the server now owns only the ciphertext, so the clear must NULL
// the *Enc sibling and leave the plaintext column alone. That is not
// cosmetic: "latitude"/"longitude" are NOT NULL on the Prisma schema, so
// a `"latitude" = NULL` clause would fail the whole UPDATE and drop the
// telemetry tick. Non-location columns (destinationName, etaMinutes, …)
// hold no coordinates and still clear in place.
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
			wantOK:       true,
			wantNulls:    []string{"destinationName", "etaMinutes"},
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
				"destinationName",
				"etaMinutes",
				"tripDistanceRemaining",
				// …location columns clear their ciphertext sibling.
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
