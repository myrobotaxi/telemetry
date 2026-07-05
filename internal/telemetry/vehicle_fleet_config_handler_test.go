package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

type stubSnapshotReader struct {
	row VehicleSnapshotRow
	err error
}

func (s *stubSnapshotReader) GetByID(_ context.Context, _ string) (VehicleSnapshotRow, error) {
	return s.row, s.err
}

func TestVehicleFleetConfigHandler(t *testing.T) {
	const (
		vehicleID = "veh_abc123"
		userID    = "user-123"
		vin       = "5YJ3E1EA1PF000001"
	)
	syncedBody := `{"response":{"synced":true,"config":{"hostname":"h","port":443}}}`
	pushBody := `{"response":{"updated_vehicles":1,"skipped_vehicles":{}}}`

	ownedRow := VehicleSnapshotRow{ID: vehicleID, UserID: userID, VIN: vin}

	tests := []struct {
		name        string
		method      string
		reader      VehicleSnapshotReader
		fleetBody   string
		fleetStatus int
		wantStatus  int
	}{
		{"GET status ok", http.MethodGet, &stubSnapshotReader{row: ownedRow}, syncedBody, http.StatusOK, http.StatusOK},
		{"POST re-push ok", http.MethodPost, &stubSnapshotReader{row: ownedRow}, pushBody, http.StatusOK, http.StatusOK},
		{"vehicle not found", http.MethodGet, &stubSnapshotReader{err: fmt.Errorf("GetByID: %w", sdk.ErrNotFound)}, syncedBody, http.StatusOK, http.StatusNotFound},
		{"ownership mismatch", http.MethodGet, &stubSnapshotReader{row: VehicleSnapshotRow{ID: vehicleID, UserID: "someone-else", VIN: vin}}, syncedBody, http.StatusOK, http.StatusForbidden},
		{"lookup internal error", http.MethodGet, &stubSnapshotReader{err: errors.New("db down")}, syncedBody, http.StatusOK, http.StatusInternalServerError},
		{"method not allowed", http.MethodPut, &stubSnapshotReader{row: ownedRow}, syncedBody, http.StatusOK, http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core := NewFleetConfigHandler(
				&stubTokenValidator{userID: userID},
				&stubVehicleOwner{ownerID: userID}, // unused on the vehicleId path
				validTeslaToken(),
				newTestFleetClient(stubFleetServer(t, tt.fleetStatus, tt.fleetBody).URL),
				EndpointConfig{Hostname: "telemetry.example.com", Port: 443},
				discardLogger(),
			)
			handler := NewVehicleFleetConfigHandler(core, tt.reader, discardLogger())

			mux := http.NewServeMux()
			mux.Handle("/api/fleet-config/vehicle/{vehicleId}", handler)

			req := httptest.NewRequestWithContext(context.Background(), tt.method, "/api/fleet-config/vehicle/"+vehicleID, nil)
			req.Header.Set("Authorization", "Bearer jwt")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status: got %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestVehicleFleetConfigHandler_MissingAuth(t *testing.T) {
	core := NewFleetConfigHandler(
		&stubTokenValidator{userID: "user-123"},
		&stubVehicleOwner{ownerID: "user-123"},
		validTeslaToken(),
		newTestFleetClient(stubFleetServer(t, http.StatusOK, `{"response":{"synced":true}}`).URL),
		EndpointConfig{Hostname: "telemetry.example.com", Port: 443},
		discardLogger(),
	)
	handler := NewVehicleFleetConfigHandler(
		core,
		&stubSnapshotReader{row: VehicleSnapshotRow{ID: "veh_1", UserID: "user-123", VIN: "5YJ3E1EA1PF000001"}},
		discardLogger(),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/fleet-config/vehicle/{vehicleId}", handler)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/fleet-config/vehicle/veh_1", nil)
	// No Authorization header.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
