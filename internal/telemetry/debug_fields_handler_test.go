package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

func TestMarshalDebugFrame_ShapeMatchesIssueSpec(t *testing.T) {
	createdAt := time.Date(2026, 4, 20, 4, 15, 0, 0, time.UTC)
	evt := events.RawVehicleTelemetryEvent{
		VIN:       "5YJ3E7EB2NF000001",
		CreatedAt: createdAt,
		Fields: []events.RawTelemetryField{
			{ProtoField: 43, ProtoName: "TimeToFullCharge", Type: "double", Value: 1.5},
			{ProtoField: 8, ProtoName: "Soc", Type: "double", Value: 78.2},
			{ProtoField: 5, ProtoName: "Odometer", Type: "invalid", Invalid: true},
		},
	}

	frame, err := marshalDebugFrame(evt)
	if err != nil {
		t.Fatalf("marshalDebugFrame: %v", err)
	}

	var decoded debugFrame
	if err := json.Unmarshal(frame, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.VIN != evt.VIN {
		t.Errorf("VIN: got %q, want %q", decoded.VIN, evt.VIN)
	}
	if decoded.Timestamp != "2026-04-20T04:15:00Z" {
		t.Errorf("Timestamp: got %q", decoded.Timestamp)
	}

	ttfc, ok := decoded.Fields["TimeToFullCharge"]
	if !ok {
		t.Fatalf("TimeToFullCharge missing from frame: %+v", decoded.Fields)
	}
	if ttfc.ProtoField != 43 || ttfc.Type != "double" || ttfc.Value != 1.5 {
		t.Errorf("TimeToFullCharge: got %+v", ttfc)
	}

	odo, ok := decoded.Fields["Odometer"]
	if !ok {
		t.Fatal("Odometer missing from frame")
	}
	if !odo.Invalid || odo.Type != "invalid" {
		t.Errorf("Odometer invalid handling: got %+v", odo)
	}
}

func TestDebugFieldsHandler_Authorize(t *testing.T) {
	tests := []struct {
		name       string
		configKey  string
		headerKey  string
		queryKey   string
		wantResult bool
	}{
		{name: "no key configured accepts anything", wantResult: true},
		{name: "matching header ok", configKey: "secret", headerKey: "secret", wantResult: true},
		{name: "matching query ok", configKey: "secret", queryKey: "secret", wantResult: true},
		{name: "missing token rejected", configKey: "secret", wantResult: false},
		{name: "wrong token rejected", configKey: "secret", headerKey: "wrong", wantResult: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &DebugFieldsHandler{cfg: DebugFieldsConfig{APIKey: tt.configKey}}
			r := newAuthRequest(tt.headerKey, tt.queryKey)
			if got := h.authorize(r); got != tt.wantResult {
				t.Errorf("authorize: got %v, want %v", got, tt.wantResult)
			}
		})
	}
}

// newAuthRequest builds a minimal *http.Request for authorize() tests.
func newAuthRequest(headerToken, queryToken string) *http.Request {
	url := "/api/debug/fields"
	if queryToken != "" {
		url += "?token=" + queryToken
	}
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if headerToken != "" {
		r.Header.Set("X-Debug-Token", headerToken)
	}
	return r
}
