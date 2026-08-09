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

	// location (MYR-515): a car that has never streamed has no fix, so the key
	// is present and null. Same nullable-object spelling as trimLabel and for
	// the same reason — this surface CAN express absence, so it does.
	got, ok = row["location"]
	if !ok {
		t.Fatalf("missing `location`; keys: %v", keysOfRow(row))
	}
	if got != nil {
		t.Errorf("location = %v (%T), want null for a car that has never streamed", got, got)
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

// TestVehiclesListHandler_LocationOnWire covers the MYR-515 position object on
// OWNER rows: present when the server holds a fix, and explicitly null when it
// does not — including the case that would actually lie.
func TestVehiclesListHandler_LocationOnWire(t *testing.T) {
	now := time.Date(2026, 8, 9, 13, 53, 0, 0, time.UTC)
	lat, lng := 37.7749, -122.4194
	zeroLat, zeroLng := 0.0, 0.0
	halfLat := 37.7749

	rows := []VehicleCatalogRow{
		{
			ID: "clloc0000000000000001", VIN: "7SAXCDE2NTF000001", Name: "Has a fix",
			Model: "Model X", Year: 2026, LastUpdated: now, Status: "parked",
			Latitude: &lat, Longitude: &lng,
		},
		{
			ID: "clloc0000000000000002", VIN: "5YJ3E1EA7TF000003", Name: "Never streamed",
			Model: "Model 3", Year: 2026, LastUpdated: now, Status: "offline",
			Latitude: nil, Longitude: nil,
		},
		{
			ID: "clloc0000000000000003", VIN: "7SAYGDEF9TF000002", Name: "Streamed 0,0",
			Model: "Model Y", Year: 2026, LastUpdated: now, Status: "parked",
			Latitude: &zeroLat, Longitude: &zeroLng,
		},
		{
			ID: "clloc0000000000000004", VIN: "5YJSA1E20TF000004", Name: "Half pair (lat only)",
			Model: "Model S", Year: 2026, LastUpdated: now, Status: "parked",
			Latitude: &halfLat, Longitude: nil,
		},
		{
			ID: "clloc0000000000000005", VIN: "5YJRE11B0TA000006", Name: "Half pair (lng only)",
			Model: "Roadster", Year: 2026, LastUpdated: now, Status: "parked",
			Latitude: nil, Longitude: &lng,
		},
	}

	resp := serveVehiclesList(t, rows)
	if len(resp) != 5 {
		t.Fatalf("want 5 items, got %d", len(resp))
	}

	// Row 0 — a real fix, emitted whole and at full precision. Full precision
	// is deliberate: the viewer mask already serves exactly this value on the
	// streaming path, so rounding here would protect nothing and would degrade
	// the short-hop pickup ETA the field exists for.
	loc, ok := resp[0]["location"]
	if !ok {
		t.Fatalf("items[0] missing `location`; keys: %v", keysOfRow(resp[0]))
	}
	obj, isObj := loc.(map[string]any)
	if !isObj {
		t.Fatalf("items[0].location = %v (%T), want an object", loc, loc)
	}
	if obj["latitude"] != lat || obj["longitude"] != lng {
		t.Errorf("items[0].location = %v, want {latitude: %v, longitude: %v} at full precision",
			obj, lat, lng)
	}

	// Row 1 — never streamed. The key is present and null.
	loc, ok = resp[1]["location"]
	if !ok {
		t.Fatalf("items[1] missing `location` — the key is always emitted; keys: %v",
			keysOfRow(resp[1]))
	}
	if loc != nil {
		t.Errorf("items[1].location = %v, want null for a car with no fix", loc)
	}

	// Row 2 — THE ASSERTION THAT MATTERS. A stored (0,0) is the §2.3 no-fix
	// sentinel, and §7.0 can express absence, so it must. Asserted in the
	// direction that would actually lie: a picker handed {0,0} does not fail
	// loudly, it confidently renders a pickup ETA to a point in the Atlantic
	// ~600km off Ghana.
	loc, ok = resp[2]["location"]
	if !ok {
		t.Fatalf("items[2] missing `location`; keys: %v", keysOfRow(resp[2]))
	}
	if loc != nil {
		t.Errorf("items[2].location = %v, want null. A stored (0,0) is the no-fix "+
			"sentinel and MUST NOT reach a consumer as a Gulf-of-Guinea coordinate", loc)
	}

	// Rows 3 and 4 — a half pair is corruption, not half a location. The
	// atomic-group rule is why `location` is one nullable object rather than
	// two nullable scalars: this state has no representation on the wire at all.
	//
	// BOTH permutations, deliberately. The guard is a two-clause `||` and a
	// single-permutation test would still pass if one clause were dropped, which
	// is precisely the edit a later refactor might make.
	for _, i := range []int{3, 4} {
		loc, ok = resp[i]["location"]
		if !ok {
			t.Fatalf("items[%d] missing `location`; keys: %v", i, keysOfRow(resp[i]))
		}
		if loc != nil {
			t.Errorf("items[%d] (%v).location = %v, want null — one axis must never be "+
				"plotted against a stale, zero, or absent mate", i, resp[i]["name"], loc)
		}
	}
}

// TestViewerSummaryCarriesLocation is the assertion MYR-515 turns on.
//
// The viewer is the party the field exists for: the picker ranks SHARED cars by
// pickup ETA, and a viewer cannot fetch a per-row /snapshot (403 by design,
// MYR-432/449). If the viewer mask ever stops projecting this, the picker
// silently loses per-row ETAs for every row but the watched one — with every
// owner-side test still green.
func TestViewerSummaryCarriesLocation(t *testing.T) {
	now := time.Date(2026, 8, 9, 13, 53, 0, 0, time.UTC)
	lat, lng := 37.7749, -122.4194
	zero := 0.0

	t.Run("a shared car with a fix", func(t *testing.T) {
		row := VehicleCatalogRow{
			ID: "clshared515000000000", VIN: "7SAXCDE2NTF000001", Name: "Amruth's X Plaid",
			Model: "Model X", Year: 2026, Color: "UltraRed", LastUpdated: now,
			Status: "parked", Latitude: &lat, Longitude: &lng,
		}

		projected := viewerSummaryMap(row, auth.ShareGrant{AllowRides: true}, now)

		loc, ok := projected["location"]
		if !ok {
			t.Fatalf("the VIEWER projection dropped `location`. This is the role the "+
				"field exists for — a viewer cannot fetch a per-row /snapshot; keys: %v",
				keysOfRow(projected))
		}
		obj, isObj := loc.(map[string]any)
		if !isObj {
			t.Fatalf("viewer location = %v (%T), want an object", loc, loc)
		}
		if obj["latitude"] != lat || obj["longitude"] != lng {
			t.Errorf("viewer location = %v, want the same full-precision pair the "+
				"viewer already receives on the streaming path", obj)
		}
	})

	t.Run("a shared car with the 0,0 sentinel serves null to the viewer too", func(t *testing.T) {
		row := VehicleCatalogRow{
			ID: "clshared515000000001", VIN: "7SAYGDEF9TF000002", Name: "Sam's Model Y",
			Model: "Model Y", Year: 2026, LastUpdated: now, Status: "parked",
			Latitude: &zero, Longitude: &zero,
		}

		projected := viewerSummaryMap(row, auth.ShareGrant{AllowRides: true}, now)

		loc, ok := projected["location"]
		if !ok {
			t.Fatalf("the VIEWER projection dropped `location`; keys: %v", keysOfRow(projected))
		}
		if loc != nil {
			t.Errorf("viewer location = %v, want null — the sentinel collapse is not an "+
				"owner-only courtesy", loc)
		}
	})
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
