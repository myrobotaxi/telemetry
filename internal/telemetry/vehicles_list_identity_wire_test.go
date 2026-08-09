package telemetry

// MYR-507 wire coverage for the catalog's vehicle-identity fields.
//
// The bug these tests exist against was NOT a decode failure or a masking
// mistake — every field involved was already on the wire, correctly, and the
// client's descriptor ladder was already correct. The rows simply carried
// nothing to identify the car with: a Go-provisioned vehicle held the
// provisioning placeholders `model: ""` / `year: 0` forever, and the trim was
// withheld from the catalog as detail-sheet-only. A rider was left with a
// colour token, so a lock screen read "UltraRed".
//
// So what is asserted here is presence-and-shape on BOTH roles: the key exists,
// nil is an explicit null rather than an empty string, and the viewer — the
// party who has no /snapshot to fall back on — sees exactly what the owner sees.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// TestVehiclesListHandler_TrimLabelOnWire covers the OWNER row: the key is
// always present, a known trim travels as a JSON string, and an unread trim
// travels as an explicit null — NEVER as "", which a descriptor builder would
// render as an empty fragment between two separators.
func TestVehiclesListHandler_TrimLabelOnWire(t *testing.T) {
	now := time.Date(2026, 8, 9, 13, 53, 0, 0, time.UTC)

	rows := []VehicleCatalogRow{
		{
			ID: "cltrim1234567890abcdef", VIN: "7SAXCDE2NTF000001", Name: "Amruth's X Plaid",
			Model: "Model X", Year: 2026, Color: "UltraRed", TrimLabel: ptr("Plaid"),
			Status: "parked", ChargeLevel: 31, EstimatedRange: 96, LastUpdated: now,
		},
		{
			ID: "clnotrim567890123abcd", VIN: "5YJ3E1EA7TF000003", Name: "Never read",
			Model: "Model 3", Year: 2026, Color: "", TrimLabel: nil,
			Status: "offline", ChargeLevel: 0, EstimatedRange: 0, LastUpdated: now,
		},
	}

	resp := serveVehiclesList(t, rows)
	if len(resp) != 2 {
		t.Fatalf("want 2 items, got %d", len(resp))
	}

	// Row 0 — a car with a known trim. Together with its model/year/color
	// siblings this row is the client's target descriptor verbatim:
	// "2026 Model X Plaid · Ultra Red" (the colour token's formatting is the
	// client's job; the server emits Tesla's own string).
	raw, ok := resp[0]["trimLabel"]
	if !ok {
		t.Fatalf("items[0] missing `trimLabel` — the key must ALWAYS be emitted; keys: %v",
			keysOfRow(resp[0]))
	}
	if got, isString := raw.(string); !isString || got != "Plaid" {
		t.Errorf("items[0].trimLabel = %v (%T), want the JSON string \"Plaid\"", raw, raw)
	}

	// Row 1 — Tesla has not been asked yet. The key must still be present, and
	// its value must be null rather than "": those mean different things, and
	// only one of them tells a consumer to drop the fragment.
	raw, ok = resp[1]["trimLabel"]
	if !ok {
		t.Fatalf("items[1] missing `trimLabel` — an unread trim is an explicit null, "+
			"not an absent key; keys: %v", keysOfRow(resp[1]))
	}
	if raw != nil {
		t.Errorf("items[1].trimLabel = %v (%T), want null. An empty string here would "+
			"read to a descriptor builder as a real (blank) trim", raw, raw)
	}

	// The identity siblings this field joins must still be on the row — the
	// descriptor needs all four, and three of them are what MYR-507 fixed
	// upstream in the DB rather than on the wire.
	for _, sibling := range []string{"model", "year", "color"} {
		if _, present := resp[0][sibling]; !present {
			t.Errorf("items[0] missing identity sibling %q — trimLabel is only useful "+
				"alongside it", sibling)
		}
	}
}

// TestVehiclesListHandler_NeverEnrichedCarServesPlaceholdersNotNulls pins the
// OTHER half of the empty-value contract, and it is deliberately the opposite
// rule from trimLabel's.
//
// `model` and `year` live on the Prisma-owned "Vehicle" row as NOT NULL
// columns, so their "not determined" spelling is `""` / `0` — the placeholders
// the provisioning INSERT used to leave behind permanently. `trimLabel` lives
// on a nullable Go-owned side table, so its spelling is `null`. Both are
// ALWAYS-EMITTED keys, and the difference between them is a consequence of
// which table each value sits on, not a disagreement. A consumer must drop the
// fragment for all three.
func TestVehiclesListHandler_NeverEnrichedCarServesPlaceholdersNotNulls(t *testing.T) {
	now := time.Date(2026, 8, 9, 4, 3, 0, 0, time.UTC)

	// A car exactly as the Go provisioning INSERT left it before MYR-507, which
	// is how the reporting vehicle actually sat in production.
	rows := []VehicleCatalogRow{{
		ID: "clunenriched00000000", VIN: "7SAXCDE2NTF000009", Name: "Tesla",
		Model: "", Year: 0, Color: "", TrimLabel: nil,
		Status: "offline", ChargeLevel: 0, EstimatedRange: 0, LastUpdated: now,
	}}

	resp := serveVehiclesList(t, rows)
	if len(resp) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp))
	}
	row := resp[0]

	// model: present, and the empty STRING — not null and not absent.
	got, ok := row["model"]
	if !ok {
		t.Fatalf("missing `model` — it is required on §7.0 and always emitted; keys: %v",
			keysOfRow(row))
	}
	if s, isString := got.(string); !isString || s != "" {
		t.Errorf("model = %v (%T), want the empty string. `model` is a NOT NULL Prisma "+
			"column: its 'not determined' spelling is \"\", never null", got, got)
	}

	// year: present, and the NUMBER zero — not null and not absent.
	got, ok = row["year"]
	if !ok {
		t.Fatalf("missing `year` — it is required on §7.0 and always emitted; keys: %v",
			keysOfRow(row))
	}
	if n, isNumber := got.(float64); !isNumber || n != 0 {
		t.Errorf("year = %v (%T), want the number 0. Same NOT NULL reasoning as `model`; "+
			"0 is never a real model year", got, got)
	}

	// trimLabel: present, and NULL — the opposite spelling, for the opposite
	// (nullable side-table) reason.
	got, ok = row["trimLabel"]
	if !ok {
		t.Fatalf("missing `trimLabel`; keys: %v", keysOfRow(row))
	}
	if got != nil {
		t.Errorf("trimLabel = %v (%T), want null. It is a NULLABLE side-table column, so "+
			"unlike `model` its 'not determined' spelling is null, not \"\"", got, got)
	}
}

// TestViewerSummaryCarriesTrimLabel is the assertion MYR-507 actually turns on.
//
// The viewer is the ONLY party who needs this field: an owner reads the trim off
// their own /snapshot, and a rider never fetches one. If the viewer mask ever
// stops projecting it, the shared-car descriptor silently regresses to the
// colour token the field report opened with — with every owner-side test still
// passing.
func TestViewerSummaryCarriesTrimLabel(t *testing.T) {
	now := time.Date(2026, 8, 9, 13, 53, 0, 0, time.UTC)

	tests := []struct {
		name      string
		trimLabel *string
		wantNull  bool
	}{
		{"a shared car with a known trim", ptr("Plaid"), false},
		{"a shared car Tesla has not been asked about", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := VehicleCatalogRow{
				ID: "clshared507000000000", VIN: "7SAXCDE2NTF000001", Name: "Amruth's X Plaid",
				Model: "Model X", Year: 2026, Color: "UltraRed", TrimLabel: tt.trimLabel,
				Status: "parked", ChargeLevel: 31, EstimatedRange: 96, LastUpdated: now,
			}

			projected := viewerSummaryMap(row, auth.ShareGrant{AllowRides: true}, now)

			got, ok := projected["trimLabel"]
			if !ok {
				t.Fatalf("the VIEWER projection dropped `trimLabel`. This is the one role "+
					"that has no /snapshot to fall back on, so dropping it here is exactly "+
					"the regression MYR-507 fixed; keys: %v", keysOfRow(projected))
			}
			switch {
			case tt.wantNull && got != nil:
				t.Errorf("viewer trimLabel = %v (%T), want null", got, got)
			case !tt.wantNull && got != "Plaid":
				t.Errorf("viewer trimLabel = %v, want %q", got, "Plaid")
			}

			// The viewer must see the identity set WHOLE. Any one of these
			// missing and the descriptor degrades again.
			for _, sibling := range []string{"model", "year", "color"} {
				if _, present := projected[sibling]; !present {
					t.Errorf("the viewer projection dropped identity sibling %q", sibling)
				}
			}
		})
	}
}

// serveVehiclesList runs the owner path end-to-end and returns the raw item
// maps, so a MISSING key stays distinguishable from a present null.
func serveVehiclesList(t *testing.T, rows []VehicleCatalogRow) []map[string]any {
	t.Helper()

	h := NewVehiclesListHandler(
		&stubTokenValidator{userID: "user-1"},
		&stubVehicleLister{rows: rows},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/vehicles", nil)
	req.Header.Set("Authorization", "Bearer valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v. Body: %s", err, rec.Body.String())
	}
	return resp.Items
}
