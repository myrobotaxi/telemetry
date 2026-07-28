package telemetry

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

// Wire-level emission tests for MYR-316. The resolver's own table
// (service_window_test.go) proves the two rules; these prove the rules actually
// reach the two surfaces, through the mask, for BOTH roles — the step where a
// missing mask entry or a forgotten toMaskMap key would silently drop the field.

// swWireEnd is the estimate carried by the fixtures below.
var swWireEnd = time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)

// TestServiceWindow_SnapshotEmission covers VehicleState.
func TestServiceWindow_SnapshotEmission(t *testing.T) {
	tests := []struct {
		name   string
		status string
		tesla  *time.Time
		owner  *time.Time
		want   any // nil means JSON null
	}{
		{
			name:   "in service with Tesla estimate",
			status: serviceStatusInService,
			tesla:  &swWireEnd,
			want:   "2026-08-01T15:00:00Z",
		},
		{
			name:   "in service falling back to the owner value",
			status: serviceStatusInService,
			owner:  &swWireEnd,
			want:   "2026-08-01T15:00:00Z",
		},
		{
			name:   "in service with no estimate emits null",
			status: serviceStatusInService,
		},
		{
			// Auto-clear on the wire: even with both columns populated, a car
			// that is not in service carries nothing.
			name:   "parked vehicle emits null despite populated columns",
			status: "parked",
			tesla:  &swWireEnd,
			owner:  &swWireEnd,
		},
	}

	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		for _, tt := range tests {
			t.Run(string(role)+"/"+tt.name, func(t *testing.T) {
				row := fixtureSnapshotRow(mediaFixtureUserID)
				row.Status = tt.status
				row.ServiceETC = tt.tesla
				row.ServiceExpectedEndAt = tt.owner

				body := decodeSnapshotBodyForRole(t, row, role)

				raw, ok := body["serviceEstimatedEndAt"]
				if !ok {
					t.Fatalf("snapshot is missing `serviceEstimatedEndAt` for role %s — "+
						"the key is ALWAYS emitted, using an explicit null for 'no estimate'", role)
				}
				if tt.want == nil {
					if raw != nil {
						t.Fatalf("serviceEstimatedEndAt = %v, want null", raw)
					}
					return
				}
				if raw != tt.want {
					t.Fatalf("serviceEstimatedEndAt = %v, want %v", raw, tt.want)
				}
			})
		}
	}
}

// TestServiceWindow_VehiclesListEmission covers VehicleSummary. Two rows in one
// response so a per-row resolution bug (rather than a whole-response one) is
// visible.
func TestServiceWindow_VehiclesListEmission(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	rows := []VehicleCatalogRow{
		{
			ID: "clsvcwin123456789abcd", VIN: "5YJ3E1EA1PF000001", Name: "AtShop",
			Model: "Model 3", Year: 2024, Color: "Blue",
			Status: serviceStatusInService, ChargeLevel: 40, EstimatedRange: 140,
			LastUpdated: now, ServiceETC: &swWireEnd,
		},
		{
			// Populated columns but NOT in service: the wire must still be null.
			ID: "clsvcwin987654321wxyz", VIN: "5YJ3E1EA1PF000002", Name: "Home",
			Model: "Model Y", Year: 2023, Color: "White",
			Status: "parked", ChargeLevel: 90, EstimatedRange: 300,
			LastUpdated: now, ServiceETC: &swWireEnd, ServiceExpectedEndAt: &swWireEnd,
		},
	}

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

	// Raw maps so a MISSING key is distinguishable from a present null.
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v. Body: %s", err, rec.Body.String())
	}
	if len(resp.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(resp.Items))
	}

	inSvc, ok := resp.Items[0]["serviceEstimatedEndAt"]
	if !ok {
		t.Fatalf("items[0] missing `serviceEstimatedEndAt`; keys: %v", keysOfRow(resp.Items[0]))
	}
	if inSvc != "2026-08-01T15:00:00Z" {
		t.Errorf("items[0].serviceEstimatedEndAt = %v, want %q", inSvc, "2026-08-01T15:00:00Z")
	}

	parked, ok := resp.Items[1]["serviceEstimatedEndAt"]
	if !ok {
		t.Fatalf("items[1] missing `serviceEstimatedEndAt`; keys: %v", keysOfRow(resp.Items[1]))
	}
	if parked != nil {
		t.Errorf("items[1].serviceEstimatedEndAt = %v, want null — a parked car carries no window", parked)
	}
}
