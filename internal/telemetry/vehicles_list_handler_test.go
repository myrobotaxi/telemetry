package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- Test doubles ---

type stubVehicleLister struct {
	rows []VehicleCatalogRow
	err  error
}

func (s *stubVehicleLister) ListByUser(_ context.Context, _ string) ([]VehicleCatalogRow, error) {
	return s.rows, s.err
}

// --- Tests ---

func TestVehiclesListHandler_ServeHTTP(t *testing.T) {
	const (
		userID    = "user-123"
		authToken = "valid-token"
	)

	now := time.Date(2026, 5, 10, 17, 45, 0, 0, time.UTC)

	owned := []VehicleCatalogRow{
		{
			ID:             "clxyz1234567890abcdef",
			VIN:            "5YJ3E1EA1PF000001",
			Name:           "Stumpy",
			Model:          "Model 3",
			Year:           2024,
			Color:          "Midnight Silver Metallic",
			Status:         "parked",
			ChargeLevel:    78,
			EstimatedRange: 245,
			LastUpdated:    now,
		},
	}

	tests := []struct {
		name           string
		authHeader     string
		tokenValidator *stubTokenValidator
		lister         *stubVehicleLister
		wantStatus     int
		wantError      string
		wantItemsLen   int
	}{
		{
			name:           "missing Authorization header",
			authHeader:     "",
			tokenValidator: &stubTokenValidator{userID: userID},
			lister:         &stubVehicleLister{},
			wantStatus:     http.StatusUnauthorized,
			wantError:      "missing Authorization header",
		},
		{
			name:           "invalid token",
			authHeader:     "Bearer bad-token",
			tokenValidator: &stubTokenValidator{err: errors.New("token expired")},
			lister:         &stubVehicleLister{},
			wantStatus:     http.StatusUnauthorized,
			wantError:      "invalid or expired token",
		},
		{
			name:           "ListByUser fails returns 500",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			lister:         &stubVehicleLister{err: errors.New("db down")},
			wantStatus:     http.StatusInternalServerError,
			wantError:      "internal error",
		},
		{
			name:           "empty owned list returns 200 with items: []",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			lister:         &stubVehicleLister{rows: nil},
			wantStatus:     http.StatusOK,
			wantItemsLen:   0,
		},
		{
			name:           "single owned vehicle returns 200 with one item",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			lister:         &stubVehicleLister{rows: owned},
			wantStatus:     http.StatusOK,
			wantItemsLen:   1,
		},
		{
			name:           "multi-row owned list returns 200 with N items",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			lister: &stubVehicleLister{rows: append([]VehicleCatalogRow{}, owned[0], VehicleCatalogRow{
				ID:             "clmno5678901234ghijkl",
				VIN:            "5YJ3E1EA1PF000002",
				Name:           "Lightning",
				Model:          "Model Y",
				Year:           2023,
				Color:          "Pearl White Multi-Coat",
				Status:         "charging",
				ChargeLevel:    42,
				EstimatedRange: 132,
				LastUpdated:    now.Add(-3 * time.Minute),
			})},
			wantStatus:   http.StatusOK,
			wantItemsLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewVehiclesListHandler(tt.tokenValidator, tt.lister, slog.New(slog.NewTextHandler(io.Discard, nil)))

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/vehicles", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d. Body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantError != "" {
				if !strings.Contains(rec.Body.String(), tt.wantError) {
					t.Errorf("body = %q, want substring %q", rec.Body.String(), tt.wantError)
				}
				return
			}

			var resp vehiclesListResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v. Body: %s", err, rec.Body.String())
			}
			if len(resp.Items) != tt.wantItemsLen {
				t.Errorf("len(items) = %d, want %d", len(resp.Items), tt.wantItemsLen)
			}
		})
	}
}

func TestVehiclesListHandler_MultiRowProjectionPreservesOrderAndFields(t *testing.T) {
	rows := []VehicleCatalogRow{
		{
			ID:             "clxyz1234567890abcdef",
			VIN:            "5YJ3E1EA1PF000001",
			Name:           "Stumpy",
			Model:          "Model 3",
			Year:           2024,
			Color:          "Midnight Silver Metallic",
			Status:         "parked",
			ChargeLevel:    78,
			EstimatedRange: 245,
			LastUpdated:    time.Date(2026, 5, 10, 17, 45, 0, 0, time.UTC),
		},
		{
			ID:             "clmno5678901234ghijkl",
			VIN:            "5YJ3E1EA1PF000002",
			Name:           "Lightning",
			Model:          "Model Y",
			Year:           2023,
			Color:          "Pearl White Multi-Coat",
			Status:         "charging",
			ChargeLevel:    42,
			EstimatedRange: 132,
			LastUpdated:    time.Date(2026, 5, 10, 17, 42, 13, 0, time.UTC),
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

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("want 2 items, got %d. Body: %s", len(resp.Items), rec.Body.String())
	}

	// Ordering preserved (same as input order — ListByUser sorts
	// ORDER BY "name", "vin" at the repo layer; the handler does not
	// re-sort, so input ordering is what the consumer sees).
	if got := resp.Items[0]["vehicleId"]; got != "clxyz1234567890abcdef" {
		t.Errorf("items[0].vehicleId = %v, want Stumpy's ID", got)
	}
	if got := resp.Items[1]["vehicleId"]; got != "clmno5678901234ghijkl" {
		t.Errorf("items[1].vehicleId = %v, want Lightning's ID", got)
	}

	// Each row is independently projected with its own field values.
	if got := resp.Items[0]["status"]; got != "parked" {
		t.Errorf("items[0].status = %v, want parked", got)
	}
	if got := resp.Items[1]["status"]; got != "charging" {
		t.Errorf("items[1].status = %v, want charging", got)
	}
	if got := resp.Items[0]["chargeLevel"]; got != float64(78) {
		t.Errorf("items[0].chargeLevel = %v, want 78", got)
	}
	if got := resp.Items[1]["chargeLevel"]; got != float64(42) {
		t.Errorf("items[1].chargeLevel = %v, want 42", got)
	}

	// VIN redaction applied per-row independently.
	if got := resp.Items[0]["vinLast4"]; got != "0001" {
		t.Errorf("items[0].vinLast4 = %v, want 0001", got)
	}
	if got := resp.Items[1]["vinLast4"]; got != "0002" {
		t.Errorf("items[1].vinLast4 = %v, want 0002", got)
	}
}

func TestVehiclesListHandler_OwnerProjection(t *testing.T) {
	// Owner role sees every VehicleSummary field with verbatim values.
	row := VehicleCatalogRow{
		ID:             "clxyz1234567890abcdef",
		VIN:            "5YJ3E1EA1PF000001",
		Name:           "Stumpy",
		Model:          "Model 3",
		Year:           2024,
		Color:          "Midnight Silver Metallic",
		Status:         "parked",
		ChargeLevel:    78,
		EstimatedRange: 245,
		LastUpdated:    time.Date(2026, 5, 10, 17, 45, 0, 0, time.UTC),
	}

	h := NewVehiclesListHandler(
		&stubTokenValidator{userID: "user-1"},
		&stubVehicleLister{rows: []VehicleCatalogRow{row}},
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
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(resp.Items))
	}

	item := resp.Items[0]

	// Every owner field is present and carries the expected value.
	wantPresent := map[string]any{
		"vehicleId":      "clxyz1234567890abcdef",
		"name":           "Stumpy",
		"model":          "Model 3",
		"year":           float64(2024), // JSON unmarshals numbers as float64
		"color":          "Midnight Silver Metallic",
		"vinLast4":       "0001",
		"status":         "parked",
		"chargeLevel":    float64(78),
		"estimatedRange": float64(245),
		"lastUpdated":    "2026-05-10T17:45:00Z",
		"role":           "owner",
	}
	for k, want := range wantPresent {
		got, ok := item[k]
		if !ok {
			t.Errorf("field %q missing from owner projection. item: %#v", k, item)
			continue
		}
		if got != want {
			t.Errorf("field %q = %v, want %v", k, got, want)
		}
	}

	// VIN is redacted to last 4 — never the full string.
	if got := item["vinLast4"]; got != "0001" {
		t.Errorf("vinLast4 = %v, want %q (last 4 only)", got, "0001")
	}
	if strings.Contains(rec.Body.String(), "5YJ3E1EA1PF000001") {
		t.Errorf("response body leaked full VIN: %s", rec.Body.String())
	}
}

func TestVehiclesListHandler_ErrorEnvelopeShape(t *testing.T) {
	// Verifies the error response matches rest-api.md §4.1.
	h := NewVehiclesListHandler(
		&stubTokenValidator{err: errors.New("expired")},
		&stubVehicleLister{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/vehicles", nil)
	req.Header.Set("Authorization", "Bearer bad")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	var env struct {
		Error struct {
			Code    string  `json:"code"`
			Message string  `json:"message"`
			SubCode *string `json:"subCode"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v. Body: %s", err, rec.Body.String())
	}
	if env.Error.Code != "auth_failed" {
		t.Errorf("error.code = %q, want auth_failed", env.Error.Code)
	}
	if env.Error.SubCode != nil {
		t.Errorf("error.subCode = %v, want null", env.Error.SubCode)
	}
}

func TestLastFourOfVIN(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"AB", "AB"},
		{"ABCD", "ABCD"},
		{"ABCDE", "BCDE"},
		{"5YJ3E1EA1PF000001", "0001"},
	}
	for _, tt := range tests {
		if got := lastFourOfVIN(tt.in); got != tt.want {
			t.Errorf("lastFourOfVIN(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
