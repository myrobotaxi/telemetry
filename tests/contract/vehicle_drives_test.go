//go:build contract

// Contract tests for GET /api/vehicles/{vehicleId}/drives
// (rest-api.md §7.2).
//
// Scope (MYR-141 Phase 1):
//   - Empty list: vehicle with zero drives -> 200 {items: [], hasMore: false}.
//   - Pagination: 25 drives, default limit 20 -> 20 items + nextCursor
//   - hasMore=true; following the cursor returns the remaining 5
//     with hasMore=false.
//   - Cursor opacity: the returned nextCursor round-trips through the
//     pagination endpoint (we don't inspect its bytes — the contract
//     guarantees opaque base64, not the encoding shape).
//   - Ordering: items are sorted (startTime DESC, id DESC) per §4.2.2.
//   - 401 missing token.
//   - 403 vehicle_not_owned.
//   - 404 not_found for unknown vehicleId. **This is the MYR-132 /
//     MYR-130 regression case** — if the route stops being mounted,
//     the response shape is the http.ServeMux 404 envelope (HTML or
//     empty body), NOT our `{"error":{"code":"not_found"...}}` JSON
//     envelope. assertErrorCode catches that drift.
package contract_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestContract_GETVehicleDrives(t *testing.T) {
	ctx := context.Background()

	const (
		ownerID    = "user_owner_drives"
		otherID    = "user_other_drives"
		vehicleID  = "veh_drives_001"
		vehicleVIN = "5YJ3E1EA1PF000301"
	)

	t.Run("empty list returns 200 with items: [] and hasMore: false", func(t *testing.T) {
		srv, seeder := setupTestServer(t)
		seeder.seedUser(ctx, t, ownerID)
		seeder.seedVehicle(ctx, t, vehicleSeed{
			ID:             vehicleID,
			UserID:         ownerID,
			VIN:            vehicleVIN,
			Name:           "No-Drives",
			Model:          "Model 3",
			Year:           2024,
			Color:          "Solid Black",
			Status:         "parked",
			ChargeLevel:    80,
			EstimatedRange: 240,
		})
		token := mintToken(t, ownerID, nil)

		resp := doGET(t, srv, "/api/vehicles/"+vehicleID+"/drives", token)
		body := readBody(t, resp)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, string(body))
		}
		var page drivesEnvelope
		decodeJSON(t, body, &page)
		if page.Items == nil {
			t.Errorf("items must be a JSON array (not null) even when empty; body: %s", string(body))
		}
		if len(page.Items) != 0 {
			t.Errorf("expected 0 items, got %d", len(page.Items))
		}
		if page.HasMore {
			t.Errorf("hasMore = true on empty list; want false")
		}
		if page.NextCursor != nil {
			t.Errorf("nextCursor = %v on empty list; want null", *page.NextCursor)
		}
	})

	t.Run("pagination round-trip: 25 drives, default limit 20", func(t *testing.T) {
		srv, seeder := setupTestServer(t)
		seeder.seedUser(ctx, t, ownerID)
		seeder.seedVehicle(ctx, t, vehicleSeed{
			ID:             vehicleID,
			UserID:         ownerID,
			VIN:            vehicleVIN,
			Name:           "Paged",
			Model:          "Model 3",
			Year:           2024,
			Color:          "Solid Black",
			Status:         "parked",
			ChargeLevel:    80,
			EstimatedRange: 240,
		})

		// Seed 25 drives with deterministic monotonically-decreasing
		// startTime so the DESC ordering is unambiguous. IDs are
		// zero-padded so the secondary id-DESC tiebreaker is stable.
		const total = 25
		for i := 0; i < total; i++ {
			// drive_24 has the latest startTime, drive_00 the earliest.
			seeder.seedDrive(ctx, t, driveSeed{
				ID:               fmt.Sprintf("drive_%02d", i),
				VehicleID:        vehicleID,
				Date:             "2026-05-01",
				StartTime:        fmt.Sprintf("2026-05-01T%02d:00:00Z", i),
				EndTime:          fmt.Sprintf("2026-05-01T%02d:30:00Z", i),
				DistanceMiles:    float64(10 + i),
				DurationMinutes:  30,
				AvgSpeedMph:      25.5,
				MaxSpeedMph:      65.0,
				StartChargeLevel: 80,
				EndChargeLevel:   78,
			})
		}

		token := mintToken(t, ownerID, nil)

		// Page 1 — no cursor; expect the 20 newest items.
		resp := doGET(t, srv, "/api/vehicles/"+vehicleID+"/drives", token)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page 1 status = %d, want 200; body: %s", resp.StatusCode, string(body))
		}
		var page1 drivesEnvelope
		decodeJSON(t, body, &page1)

		if len(page1.Items) != 20 {
			t.Errorf("page 1 items = %d, want 20", len(page1.Items))
		}
		if !page1.HasMore {
			t.Errorf("page 1 hasMore = false, want true (25 drives total > 20 limit)")
		}
		if page1.NextCursor == nil || *page1.NextCursor == "" {
			t.Fatalf("page 1 nextCursor missing; want opaque base64 string")
		}

		// Ordering: startTime DESC, id DESC per §4.2.2. The newest
		// item (drive_24) must be first; the 20th-newest (drive_05)
		// must be last.
		if got := page1.Items[0]["id"]; got != "drive_24" {
			t.Errorf("page 1 items[0].id = %v, want drive_24", got)
		}
		if got := page1.Items[19]["id"]; got != "drive_05" {
			t.Errorf("page 1 items[19].id = %v, want drive_05", got)
		}
		assertStartTimesDescending(t, page1.Items)

		// Page 2 — follow the cursor; expect the remaining 5 with
		// hasMore=false. The cursor is treated as opaque: we don't
		// decode it, we just round-trip it back into the next request.
		nextURL := "/api/vehicles/" + vehicleID + "/drives?cursor=" + *page1.NextCursor
		resp2 := doGET(t, srv, nextURL, token)
		body2 := readBody(t, resp2)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("page 2 status = %d, want 200; body: %s", resp2.StatusCode, string(body2))
		}
		var page2 drivesEnvelope
		decodeJSON(t, body2, &page2)

		if len(page2.Items) != 5 {
			t.Errorf("page 2 items = %d, want 5", len(page2.Items))
		}
		if page2.HasMore {
			t.Errorf("page 2 hasMore = true, want false (25 - 20 = 5 remaining)")
		}
		if page2.NextCursor != nil {
			t.Errorf("page 2 nextCursor = %v, want null (final page)", *page2.NextCursor)
		}
		if got := page2.Items[0]["id"]; got != "drive_04" {
			t.Errorf("page 2 items[0].id = %v, want drive_04", got)
		}
		if got := page2.Items[4]["id"]; got != "drive_00" {
			t.Errorf("page 2 items[4].id = %v, want drive_00", got)
		}
	})

	t.Run("missing Authorization header returns 401", func(t *testing.T) {
		srv, seeder := setupTestServer(t)
		seeder.seedUser(ctx, t, ownerID)
		seeder.seedVehicle(ctx, t, vehicleSeed{
			ID:     vehicleID,
			UserID: ownerID,
			VIN:    vehicleVIN,
			Model:  "Model 3", Year: 2024, Color: "Black",
			Status: "parked",
		})

		resp := doGET(t, srv, "/api/vehicles/"+vehicleID+"/drives", "")
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body: %s", resp.StatusCode, string(body))
		}
		assertErrorCode(t, body, "auth_failed")
	})

	t.Run("vehicle owned by another user returns 403 vehicle_not_owned", func(t *testing.T) {
		srv, seeder := setupTestServer(t)
		seeder.seedUser(ctx, t, ownerID)
		seeder.seedUser(ctx, t, otherID)
		seeder.seedVehicle(ctx, t, vehicleSeed{
			ID:     vehicleID,
			UserID: otherID, // owned by someone else
			VIN:    vehicleVIN,
			Model:  "Model 3", Year: 2024, Color: "Black",
			Status: "parked",
		})

		resp := doGET(t, srv, "/api/vehicles/"+vehicleID+"/drives", mintToken(t, ownerID, nil))
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body: %s", resp.StatusCode, string(body))
		}
		assertErrorCode(t, body, "vehicle_not_owned")
	})

	t.Run("MYR-145: items include start/end location + address when populated, and omit when empty", func(t *testing.T) {
		srv, seeder := setupTestServer(t)
		seeder.seedUser(ctx, t, ownerID)
		seeder.seedVehicle(ctx, t, vehicleSeed{
			ID:             vehicleID,
			UserID:         ownerID,
			VIN:            vehicleVIN,
			Name:           "Located",
			Model:          "Model 3",
			Year:           2024,
			Color:          "Solid Black",
			Status:         "parked",
			ChargeLevel:    80,
			EstimatedRange: 240,
		})

		// Drive 1: fully geocoded — all four location fields populated.
		seeder.seedDrive(ctx, t, driveSeed{
			ID:               "drive_loc_complete",
			VehicleID:        vehicleID,
			Date:             "2026-05-01",
			StartTime:        "2026-05-01T09:00:00Z",
			EndTime:          "2026-05-01T09:30:00Z",
			StartLocation:    "Home",
			StartAddress:     "742 Evergreen Terrace, San Francisco, CA 94107",
			EndLocation:      "Whole Foods Market",
			EndAddress:       "399 4th Street, San Francisco, CA 94107",
			DistanceMiles:    12.4,
			DurationMinutes:  30,
			AvgSpeedMph:      25.5,
			MaxSpeedMph:      65.0,
			StartChargeLevel: 80,
			EndChargeLevel:   78,
		})

		// Drive 2: not yet geocoded — every location field empty.
		// startTime is earlier so it sorts after the geocoded drive.
		seeder.seedDrive(ctx, t, driveSeed{
			ID:               "drive_loc_empty",
			VehicleID:        vehicleID,
			Date:             "2026-05-01",
			StartTime:        "2026-05-01T07:00:00Z",
			EndTime:          "2026-05-01T07:30:00Z",
			DistanceMiles:    5.0,
			DurationMinutes:  30,
			AvgSpeedMph:      10.0,
			MaxSpeedMph:      25.0,
			StartChargeLevel: 85,
			EndChargeLevel:   83,
		})

		token := mintToken(t, ownerID, nil)
		resp := doGET(t, srv, "/api/vehicles/"+vehicleID+"/drives", token)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, string(body))
		}

		var page drivesEnvelope
		decodeJSON(t, body, &page)
		if len(page.Items) != 2 {
			t.Fatalf("items = %d, want 2", len(page.Items))
		}

		// Newest first per (startTime DESC, id DESC) — drive_loc_complete
		// has the later startTime, so it lands at index 0.
		populated := page.Items[0]
		if got := populated["id"]; got != "drive_loc_complete" {
			t.Fatalf("items[0].id = %v, want drive_loc_complete (ordering check)", got)
		}
		for _, kv := range []struct {
			key  string
			want string
		}{
			{"startLocation", "Home"},
			{"startAddress", "742 Evergreen Terrace, San Francisco, CA 94107"},
			{"endLocation", "Whole Foods Market"},
			{"endAddress", "399 4th Street, San Francisco, CA 94107"},
		} {
			got, ok := populated[kv.key]
			if !ok {
				t.Errorf("items[0].%s: missing, want %q (MYR-145)", kv.key, kv.want)
				continue
			}
			if got != kv.want {
				t.Errorf("items[0].%s = %v, want %q", kv.key, got, kv.want)
			}
		}

		empty := page.Items[1]
		if got := empty["id"]; got != "drive_loc_empty" {
			t.Fatalf("items[1].id = %v, want drive_loc_empty", got)
		}
		// MYR-145 nullable convention: drop the key entirely when the
		// store row carries the empty-string default.
		for _, k := range []string{"startLocation", "startAddress", "endLocation", "endAddress"} {
			if _, ok := empty[k]; ok {
				t.Errorf("items[1].%s: present on the wire; want omitted when DB row is empty (MYR-145)", k)
			}
		}
	})

	t.Run("unknown vehicleId returns 404 not_found (MYR-132 regression)", func(t *testing.T) {
		// The point of this case: if a future refactor un-mounts
		// the route, http.ServeMux 404s with `404 page not found`
		// (text/plain) instead of our typed JSON envelope. The
		// assertErrorCode below decodes JSON and asserts on
		// error.code; both halves fail if the route is missing.
		srv, seeder := setupTestServer(t)
		seeder.seedUser(ctx, t, ownerID)

		resp := doGET(t, srv, "/api/vehicles/veh_does_not_exist/drives", mintToken(t, ownerID, nil))
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body: %s", resp.StatusCode, string(body))
		}
		assertErrorCode(t, body, "not_found")
	})
}

// drivesEnvelope is the local decode shape for the drives-page
// response. Items stay as map[string]any so individual assertions can
// inspect arbitrary fields without bringing in a full DriveSummary
// struct (which would couple this file to internal/telemetry types).
type drivesEnvelope struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"nextCursor"`
	HasMore    bool             `json:"hasMore"`
}

// assertStartTimesDescending walks the page and fails if any item's
// startTime is greater than the previous item's. The ISO 8601 strings
// sort lexicographically the same way they sort chronologically, so a
// string compare is sufficient.
func assertStartTimesDescending(t *testing.T, items []map[string]any) {
	t.Helper()
	prev := ""
	for i, item := range items {
		st, _ := item["startTime"].(string)
		if i > 0 && st > prev {
			t.Errorf("items[%d].startTime (%s) > items[%d].startTime (%s); want DESC order",
				i, st, i-1, prev)
			return
		}
		prev = st
	}
}
