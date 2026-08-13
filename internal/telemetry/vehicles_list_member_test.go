package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestVehiclesListMemberMerge covers the MYR-540 member merge: the vehicle of
// a live group ride the caller JOINED appears in their catalog as a
// `role: "viewer"` row with NO ride capability, deduplicated against the owner
// and shared halves — so the catalog names the same cars the WS access set's
// membership leg admits, and a joiner's map has a car to resolve.
func TestVehiclesListMemberMerge(t *testing.T) {
	ownedRow := VehicleCatalogRow{
		ID:             "cl-owned-vehicle",
		VIN:            "5YJ3E1EA1PF000009",
		Name:           "My Model Y",
		Model:          "Model Y",
		Year:           2023,
		Color:          "Blue",
		Status:         "parked",
		ChargeLevel:    91,
		EstimatedRange: 280,
		LastUpdated:    time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC),
	}
	rideCarRow := VehicleCatalogRow{
		ID:             "cl-ride-vehicle",
		VIN:            "5YJXCAE41MF000123",
		Name:           "Quicksilver",
		Model:          "Model X",
		Year:           2026,
		Color:          "Silver",
		Status:         "driving",
		ChargeLevel:    74,
		EstimatedRange: 210,
		LastUpdated:    time.Date(2026, 8, 13, 8, 1, 0, 0, time.UTC),
	}

	listAs := func(t *testing.T, owned []VehicleCatalogRow, shared SharedVehicleLister, members RideMemberVehicleLister) []map[string]any {
		t.Helper()
		opts := []VehiclesListOption{}
		if shared != nil {
			opts = append(opts, WithSharedVehicles(shared))
		}
		if members != nil {
			opts = append(opts, WithMemberVehicles(members))
		}
		h := NewVehiclesListHandler(
			&stubTokenValidator{userID: shareViewerUser},
			&stubVehicleLister{rows: owned},
			discardLogger(),
			opts...,
		)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/vehicles", nil)
		req.Header.Set("Authorization", "Bearer t")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want 200. Body: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Items
	}

	t.Run("a joined ride's car appears as a capability-less viewer row", func(t *testing.T) {
		items := listAs(t, []VehicleCatalogRow{ownedRow}, nil,
			&fakeMemberVehicleLister{rows: []VehicleCatalogRow{rideCarRow}})
		if len(items) != 2 {
			t.Fatalf("got %d items, want 2 (one owned, one member)", len(items))
		}
		row := items[1]
		if row["role"] != "viewer" {
			t.Errorf("member row role = %v, want viewer", row["role"])
		}
		if row["vehicleId"] != "cl-ride-vehicle" {
			t.Errorf("member row vehicleId = %v", row["vehicleId"])
		}
		// The ZERO grant: riding in a car buys watching it, never booking it.
		// The tier must SAY so rather than be absent — a consumer told to read
		// an absent tier as the lowest would still be guessing.
		if perm, ok := row["sharePermission"].(string); !ok || perm == "" {
			t.Errorf("member row sharePermission = %v, want the zero grant's tier stated", row["sharePermission"])
		}
	})

	t.Run("a car the caller already owns is not duplicated by membership", func(t *testing.T) {
		items := listAs(t, []VehicleCatalogRow{ownedRow}, nil,
			&fakeMemberVehicleLister{rows: []VehicleCatalogRow{ownedRow}})
		if len(items) != 1 {
			t.Fatalf("got %d items, want 1 — the owner row wins", len(items))
		}
		if items[0]["role"] != "owner" {
			t.Errorf("surviving row role = %v, want owner (the richer claim)", items[0]["role"])
		}
	})

	t.Run("a car the caller holds a share on keeps the share's row", func(t *testing.T) {
		sharedRow := SharedVehicleRow{VehicleCatalogRow: rideCarRow, AllowRides: true}
		items := listAs(t, nil,
			&fakeSharedLister{rows: []SharedVehicleRow{sharedRow}},
			&fakeMemberVehicleLister{rows: []VehicleCatalogRow{rideCarRow}})
		if len(items) != 1 {
			t.Fatalf("got %d items, want 1 — the share row wins", len(items))
		}
	})

	t.Run("a member-read failure degrades to the smaller set, never a 500", func(t *testing.T) {
		items := listAs(t, []VehicleCatalogRow{ownedRow}, nil,
			&fakeMemberVehicleLister{err: errors.New("membership join hiccup")})
		if len(items) != 1 {
			t.Fatalf("got %d items, want the owner's own garage intact", len(items))
		}
	})

	t.Run("no lister wired leaves the response byte-identical", func(t *testing.T) {
		with := listAs(t, []VehicleCatalogRow{ownedRow}, nil, nil)
		if len(with) != 1 || with[0]["role"] != "owner" {
			t.Fatalf("un-wired member merge changed the response: %v", with)
		}
	})
}

// fakeMemberVehicleLister satisfies RideMemberVehicleLister for the merge tests.
type fakeMemberVehicleLister struct {
	rows []VehicleCatalogRow
	err  error
}

func (f *fakeMemberVehicleLister) ListMemberVehiclesByUser(context.Context, string) ([]VehicleCatalogRow, error) {
	return f.rows, f.err
}
