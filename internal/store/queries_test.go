package store

import (
	"strings"
	"testing"
	"time"
)

func TestBuildTelemetryUpdate_ClearFields(t *testing.T) {
	tests := []struct {
		name         string
		vin          string
		update       VehicleUpdate
		wantOK       bool
		wantNulls    []string // column names expected in "col" = NULL clauses
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
			wantOK:    true,
			wantNulls: []string{"originLatitude", "originLongitude"},
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
				"destinationName",
				"etaMinutes",
				"tripDistanceRemaining",
				"destinationLatitude",
				"destinationLongitude",
				"originLatitude",
				"originLongitude",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, args, ok := buildTelemetryUpdate(tt.vin, tt.update)
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, _, ok := buildTelemetryUpdate("TEST_VIN", tt.update)
			if !ok {
				t.Fatal("expected ok=true")
			}
			if !strings.Contains(query, `"`+tt.wantCol+`"`) {
				t.Errorf("query missing column %q:\n%s", tt.wantCol, query)
			}
		})
	}
}
