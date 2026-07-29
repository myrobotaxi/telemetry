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

// TestVehiclesListViewerMerge covers the MYR-91 viewer merge delivered by
// MYR-184: shared vehicles appear in the caller's catalog as `role: "viewer"`
// rows carrying their tier, while the caller's own cars are untouched.
func TestVehiclesListViewerMerge(t *testing.T) {
	ownedRow := VehicleCatalogRow{
		ID:             "cl-owned-vehicle",
		VIN:            "5YJ3E1EA1PF000009",
		Name:           "My Model Y",
		Model:          "Model Y",
		Year:           2023,
		Color:          "Blue",
		LicensePlate:   "7XYZ890",
		Status:         "parked",
		ChargeLevel:    91,
		EstimatedRange: 280,
		LastUpdated:    time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC),
	}

	listAs := func(t *testing.T, owned []VehicleCatalogRow, shared SharedVehicleLister) []map[string]any {
		t.Helper()
		opts := []VehiclesListOption{}
		if shared != nil {
			opts = append(opts, WithSharedVehicles(shared))
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

	t.Run("owner rows are unchanged and carry no sharePermission", func(t *testing.T) {
		items := listAs(t, []VehicleCatalogRow{ownedRow}, &fakeSharedLister{})
		if len(items) != 1 {
			t.Fatalf("got %d items, want 1", len(items))
		}
		row := items[0]
		if row["role"] != "owner" {
			t.Errorf("role = %v, want owner", row["role"])
		}
		if _, present := row["sharePermission"]; present {
			t.Error("an OWNER row carried sharePermission; an owner is not on a tier, and a consumer told to read an absent tier as the lowest would misread it")
		}
		if row["name"] != "My Model Y" {
			t.Errorf("the owner's own nickname was masked away: %v", row["name"])
		}
	})

	t.Run("shared rows are appended as viewer rows with their tier", func(t *testing.T) {
		shared := &fakeSharedLister{rows: []SharedVehicleRow{sharedCatalogRow("live_history")}}
		items := listAs(t, []VehicleCatalogRow{ownedRow}, shared)
		if len(items) != 2 {
			t.Fatalf("got %d items, want 2 (one owned, one shared)", len(items))
		}

		// Owner rows come first — a caller's own cars lead their garage.
		if items[0]["role"] != "owner" {
			t.Errorf("items[0].role = %v, want owner", items[0]["role"])
		}

		viewerRow := items[1]
		if viewerRow["role"] != "viewer" {
			t.Errorf("items[1].role = %v, want viewer", viewerRow["role"])
		}
		if viewerRow["sharePermission"] != "live_history" {
			t.Errorf("sharePermission = %v, want live_history", viewerRow["sharePermission"])
		}
		if _, leaked := viewerRow["name"]; leaked {
			t.Error("the owner-curated `name` reached a viewer row — the viewer mask was not applied")
		}
		if viewerRow["licensePlate"] == nil {
			t.Error("a viewer row is missing licensePlate; a rider identifies the car at pickup from it")
		}
	})

	t.Run("without the merge wired the endpoint stays owner-only", func(t *testing.T) {
		// The fail-closed default: a deployment that forgot the option
		// under-reports rather than over-shares.
		items := listAs(t, []VehicleCatalogRow{ownedRow}, nil)
		if len(items) != 1 {
			t.Fatalf("got %d items, want 1", len(items))
		}
	})

	t.Run("a shared-lookup failure degrades to owner rows, never to a 500", func(t *testing.T) {
		shared := &fakeSharedLister{err: errors.New("connection refused")}
		items := listAs(t, []VehicleCatalogRow{ownedRow}, shared)
		if len(items) != 1 {
			t.Fatalf("got %d items, want the caller's own car — a sharing hiccup must not take their garage down", len(items))
		}
		if items[0]["role"] != "owner" {
			t.Errorf("items[0].role = %v", items[0]["role"])
		}
	})

	t.Run("a caller with only shared cars still gets them", func(t *testing.T) {
		shared := &fakeSharedLister{rows: []SharedVehicleRow{sharedCatalogRow("live")}}
		items := listAs(t, nil, shared)
		if len(items) != 1 {
			t.Fatalf("got %d items, want 1 — this is the rider who owns nothing and was invited to one car", len(items))
		}
		if items[0]["role"] != "viewer" || items[0]["sharePermission"] != "live" {
			t.Errorf("row = %v", items[0])
		}
	})
}
