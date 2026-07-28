package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFleetAPIClient_GetServiceData_Decode covers the decode contract. The
// ALL-NULL case is the one that matters most: Tesla returns an all-null
// service_data body for a visit with no appointment record, and that is COMMON
// AND NORMAL — not an error and not a fetch failure. A decoder that treated it
// as malformed, or that collapsed null to a zero time, would either drop the
// owner-entered fallback on the floor or emit an estimate of year 1.
func TestFleetAPIClient_GetServiceData_Decode(t *testing.T) {
	t.Parallel()

	wantETC := time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		body        string
		wantETC     *time.Time
		wantStatus  *string
		wantVisitNo *string
		wantStatID  *int
	}{
		{
			name: "fully populated visit",
			body: `{"response":{"service_status":"in_service","service_etc":"2026-08-01T15:00:00Z",` +
				`"service_visit_number":"SV-12345","status_id":2}}`,
			wantETC:     &wantETC,
			wantStatus:  strPtr("in_service"),
			wantVisitNo: strPtr("SV-12345"),
			wantStatID:  ptrInt(2),
		},
		{
			// The verified-live shape for a visit with no appointment record.
			name: "all null",
			body: `{"response":{"service_status":null,"service_etc":null,` +
				`"service_visit_number":null,"status_id":null}}`,
		},
		{
			// Tesla omitting the keys entirely must behave identically to
			// explicit nulls — neither is an error.
			name: "empty response object",
			body: `{"response":{}}`,
		},
		{
			name:       "estimate absent but visit known",
			body:       `{"response":{"service_status":"in_service","service_etc":null,"status_id":2}}`,
			wantStatus: strPtr("in_service"),
			wantStatID: ptrInt(2),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			client := NewFleetAPIClient(FleetAPIConfig{BaseURL: srv.URL}, fleetTestLogger())
			data, err := client.GetServiceData(context.Background(), "token", testVIN)
			if err != nil {
				t.Fatalf("GetServiceData: %v", err)
			}

			if want := "/api/1/vehicles/" + testVIN + "/service_data"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}

			assertTimePtr(t, "service_etc", data.ServiceETC, tt.wantETC)
			assertStrPtr(t, "service_status", data.ServiceStatus, tt.wantStatus)
			assertStrPtr(t, "service_visit_number", data.ServiceVisitNumber, tt.wantVisitNo)
			assertIntPtr(t, "status_id", data.StatusID, tt.wantStatID)
		})
	}
}

// The guards must reject before any network call, and must never put a raw VIN
// in the error string.
func TestFleetAPIClient_GetServiceData_InputGuards(t *testing.T) {
	t.Parallel()

	client := NewFleetAPIClient(FleetAPIConfig{BaseURL: "http://127.0.0.1:1"}, fleetTestLogger())

	t.Run("empty token", func(t *testing.T) {
		t.Parallel()
		if _, err := client.GetServiceData(context.Background(), "", testVIN); err == nil {
			t.Fatal("expected an error for an empty token")
		}
	})

	t.Run("short VIN is rejected and redacted", func(t *testing.T) {
		t.Parallel()
		const badVIN = "TOOSHORT"
		_, err := client.GetServiceData(context.Background(), "token", badVIN)
		if err == nil {
			t.Fatal("expected an error for an invalid VIN")
		}
		if strings.Contains(err.Error(), badVIN) {
			t.Errorf("error leaked the raw VIN: %v", err)
		}
	})
}

// A non-2xx response surfaces as a *FleetAPIError so callers can classify it —
// notably the 408 that means "the car went back to sleep".
func TestFleetAPIClient_GetServiceData_UpstreamError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = w.Write([]byte(`{"error":"vehicle unavailable"}`))
	}))
	t.Cleanup(srv.Close)

	client := NewFleetAPIClient(FleetAPIConfig{BaseURL: srv.URL}, fleetTestLogger())
	if _, err := client.GetServiceData(context.Background(), "token", testVIN); err == nil {
		t.Fatal("expected an error for a 408 response")
	} else if !isFleetStatus(err, http.StatusRequestTimeout) {
		t.Fatalf("error did not carry the Fleet 408: %v", err)
	}
}

// --- small pointer helpers, local to this file ---

func strPtr(s string) *string { return &s }

func ptrInt(i int) *int { return &i }

func assertTimePtr(t *testing.T, field string, got, want *time.Time) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %v, want nil", field, got)
	case want != nil && got == nil:
		t.Errorf("%s = nil, want %v", field, want)
	case want != nil && !got.Equal(*want):
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}

func assertStrPtr(t *testing.T, field string, got, want *string) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %q, want nil", field, *got)
	case want != nil && got == nil:
		t.Errorf("%s = nil, want %q", field, *want)
	case want != nil && *got != *want:
		t.Errorf("%s = %q, want %q", field, *got, *want)
	}
}

func assertIntPtr(t *testing.T, field string, got, want *int) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %d, want nil", field, *got)
	case want != nil && got == nil:
		t.Errorf("%s = nil, want %d", field, *want)
	case want != nil && *got != *want:
		t.Errorf("%s = %d, want %d", field, *got, *want)
	}
}
