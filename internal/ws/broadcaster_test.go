package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

// stubVINResolver is a test double that maps VINs to vehicle IDs.
type stubVINResolver struct {
	mu       sync.RWMutex
	mapping  map[string]string
	err      error
	callLog  []string
}

func newStubVINResolver(mapping map[string]string) *stubVINResolver {
	return &stubVINResolver{mapping: mapping}
}

func (r *stubVINResolver) GetByVIN(_ context.Context, vin string) (string, error) {
	r.mu.Lock()
	r.callLog = append(r.callLog, vin)
	r.mu.Unlock()

	if r.err != nil {
		return "", r.err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.mapping[vin]
	if !ok {
		return "", errors.New("VIN not found: " + vin)
	}
	return id, nil
}

func ptrFloat64(v float64) *float64 { return &v }
func ptrInt64(v int64) *int64       { return &v }
func ptrString(v string) *string    { return &v }
func ptrBool(v bool) *bool          { return &v }

func TestBroadcaster_HandleTelemetry(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{
		"5YJ3E1EA1NF000001": "vehicle-id-1",
	})

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	now := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	event := events.NewEvent(events.VehicleTelemetryEvent{
		VIN:       "5YJ3E1EA1NF000001",
		CreatedAt: now,
		Fields: map[string]events.TelemetryValue{
			"speed":          {FloatVal: ptrFloat64(65.5)},
			"soc":            {IntVal: ptrInt64(78)},
			"heading":        {FloatVal: ptrFloat64(180.0)},
			"gear":           {StringVal: ptrString("D")},
			"estimatedRange": {FloatVal: ptrFloat64(200.5)},
			"insideTemp":     {FloatVal: ptrFloat64(72.0)},
			"outsideTemp":    {FloatVal: ptrFloat64(55.0)},
			"odometer":       {FloatVal: ptrFloat64(12345.6)},
			"location": {LocationVal: &events.Location{
				Latitude:  37.7749,
				Longitude: -122.4194,
			}},
		},
	})

	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Give the subscriber goroutine time to process.
	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > 0
	})

	// The hub has no connected clients, so we can't read from a client.
	// Instead, verify the resolver was called with the correct VIN.
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	if len(resolver.callLog) == 0 {
		t.Fatal("expected VIN resolver to be called")
	}
	if resolver.callLog[0] != "5YJ3E1EA1NF000001" {
		t.Fatalf("expected VIN 5YJ3E1EA1NF000001, got %q", resolver.callLog[0])
	}
}

func TestBroadcaster_HandleDriveStarted(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{
		"5YJ3E1EA1NF000001": "vehicle-id-1",
	})

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	now := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	event := events.NewEvent(events.DriveStartedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive-abc",
		Location: events.Location{
			Latitude:  37.7749,
			Longitude: -122.4194,
		},
		StartedAt: now,
	})

	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > 0
	})
}

func TestBroadcaster_HandleDriveEnded(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{
		"5YJ3E1EA1NF000001": "vehicle-id-1",
	})

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	now := time.Date(2026, 3, 18, 12, 30, 0, 0, time.UTC)
	event := events.NewEvent(events.DriveEndedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive-abc",
		Stats: events.DriveStats{
			Distance: 15.3,
			Duration: 25 * time.Minute,
			AvgSpeed: 36.7,
			MaxSpeed: 65.0,
		},
		EndedAt: now,
	})

	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > 0
	})
}

func TestBroadcaster_HandleConnectivity(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{
		"5YJ3E1EA1NF000001": "vehicle-id-1",
	})

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	now := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	event := events.NewEvent(events.ConnectivityEvent{
		VIN:       "5YJ3E1EA1NF000001",
		Status:    events.StatusConnected,
		Timestamp: now,
	})

	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > 0
	})
}

func TestBroadcaster_VINResolutionFailure_SkipsEvent(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{})
	resolver.err = errors.New("database unavailable")

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	event := events.NewEvent(events.VehicleTelemetryEvent{
		VIN:       "UNKNOWN_VIN",
		CreatedAt: time.Now(),
		Fields: map[string]events.TelemetryValue{
			"speed": {FloatVal: ptrFloat64(50.0)},
		},
	})

	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Give subscriber time to process.
	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > 0
	})

	// The event should have been processed (resolver called) but no
	// broadcast should have happened since resolution failed.
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	if len(resolver.callLog) != 1 {
		t.Fatalf("expected 1 resolver call, got %d", len(resolver.callLog))
	}
}

func TestBroadcaster_Stop_Unsubscribes(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{
		"5YJ3E1EA1NF000001": "vehicle-id-1",
	})

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if len(b.subs) != 4 {
		t.Fatalf("expected 4 subscriptions, got %d", len(b.subs))
	}

	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(b.subs) != 0 {
		t.Fatalf("expected 0 subscriptions after stop, got %d", len(b.subs))
	}
}

func TestFieldMapping(t *testing.T) {
	tests := []struct {
		name     string
		fields   map[string]events.TelemetryValue
		wantKeys []string
		check    func(t *testing.T, result map[string]any)
	}{
		{
			name: "speed maps directly",
			fields: map[string]events.TelemetryValue{
				"speed": {FloatVal: ptrFloat64(65.5)},
			},
			wantKeys: []string{"speed"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				assertInt(t, result, "speed", 66)
			},
		},
		{
			name: "location splits into latitude and longitude",
			fields: map[string]events.TelemetryValue{
				"location": {LocationVal: &events.Location{
					Latitude:  37.7749,
					Longitude: -122.4194,
				}},
			},
			wantKeys: []string{"latitude", "longitude"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				assertFloat(t, result, "latitude", 37.7749)
				assertFloat(t, result, "longitude", -122.4194)
			},
		},
		{
			name: "heading maps directly",
			fields: map[string]events.TelemetryValue{
				"heading": {FloatVal: ptrFloat64(180.0)},
			},
			wantKeys: []string{"heading"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				assertInt(t, result, "heading", 180)
			},
		},
		{
			name: "gear maps to gearPosition",
			fields: map[string]events.TelemetryValue{
				"gear": {StringVal: ptrString("D")},
			},
			wantKeys: []string{"gearPosition"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				if result["gearPosition"] != "D" {
					t.Fatalf("expected gearPosition=D, got %v", result["gearPosition"])
				}
			},
		},
		{
			name: "soc maps to chargeLevel",
			fields: map[string]events.TelemetryValue{
				"soc": {IntVal: ptrInt64(78)},
			},
			wantKeys: []string{"chargeLevel"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				// JSON numbers come through as float64 via any
				got, ok := result["chargeLevel"].(int64)
				if !ok {
					t.Fatalf("chargeLevel not int64, got %T", result["chargeLevel"])
				}
				if got != 78 {
					t.Fatalf("expected chargeLevel=78, got %d", got)
				}
			},
		},
		{
			name: "estimatedRange maps directly",
			fields: map[string]events.TelemetryValue{
				"estimatedRange": {FloatVal: ptrFloat64(200.5)},
			},
			wantKeys: []string{"estimatedRange"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				assertInt(t, result, "estimatedRange", 201)
			},
		},
		{
			name: "insideTemp maps to interiorTemp",
			fields: map[string]events.TelemetryValue{
				"insideTemp": {FloatVal: ptrFloat64(72.0)},
			},
			wantKeys: []string{"interiorTemp"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				assertInt(t, result, "interiorTemp", 72)
			},
		},
		{
			name: "outsideTemp maps to exteriorTemp",
			fields: map[string]events.TelemetryValue{
				"outsideTemp": {FloatVal: ptrFloat64(55.0)},
			},
			wantKeys: []string{"exteriorTemp"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				assertInt(t, result, "exteriorTemp", 55)
			},
		},
		{
			name: "odometer maps to odometerMiles",
			fields: map[string]events.TelemetryValue{
				"odometer": {FloatVal: ptrFloat64(12345.6)},
			},
			wantKeys: []string{"odometerMiles"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				assertInt(t, result, "odometerMiles", 12346)
			},
		},
		{
			name: "bool value unwrapped",
			fields: map[string]events.TelemetryValue{
				"locked": {BoolVal: ptrBool(true)},
			},
			wantKeys: []string{"locked"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				if result["locked"] != true {
					t.Fatalf("expected locked=true, got %v", result["locked"])
				}
			},
		},
		{
			name: "destinationLocation splits into lat/lng",
			fields: map[string]events.TelemetryValue{
				"destinationLocation": {LocationVal: &events.Location{
					Latitude:  40.7128,
					Longitude: -74.0060,
				}},
			},
			wantKeys: []string{"destinationLatitude", "destinationLongitude"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				assertFloat(t, result, "destinationLatitude", 40.7128)
				assertFloat(t, result, "destinationLongitude", -74.0060)
			},
		},
		{
			name: "originLocation splits into lat/lng",
			fields: map[string]events.TelemetryValue{
				"originLocation": {LocationVal: &events.Location{
					Latitude:  37.7749,
					Longitude: -122.4194,
				}},
			},
			wantKeys: []string{"originLatitude", "originLongitude"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				assertFloat(t, result, "originLatitude", 37.7749)
				assertFloat(t, result, "originLongitude", -122.4194)
			},
		},
		{
			name: "nil destinationLocation is skipped",
			fields: map[string]events.TelemetryValue{
				"destinationLocation": {},
			},
			wantKeys: nil,
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				if _, ok := result["destinationLatitude"]; ok {
					t.Fatal("expected no destinationLatitude key for nil location")
				}
			},
		},
		{
			name: "milesToArrival maps to tripDistanceRemaining",
			fields: map[string]events.TelemetryValue{
				"milesToArrival": {FloatVal: ptrFloat64(42.5)},
			},
			wantKeys: []string{"tripDistanceRemaining"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				got, ok := result["tripDistanceRemaining"].(float64)
				if !ok {
					t.Fatalf("expected tripDistanceRemaining to be float64, got %T", result["tripDistanceRemaining"])
				}
				if got != 42.5 {
					t.Fatalf("expected tripDistanceRemaining=42.5, got %v", got)
				}
			},
		},
		{
			name: "routeLine decodes to routeCoordinates in lng/lat order",
			fields: map[string]events.TelemetryValue{
				"routeLine": {StringVal: ptrString("_p~iF~ps|U_ulLnnqC_mqNvxq`@")},
			},
			wantKeys: []string{"routeCoordinates"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				coords, ok := result["routeCoordinates"].([][]float64)
				if !ok {
					t.Fatalf("expected routeCoordinates to be [][]float64, got %T", result["routeCoordinates"])
				}
				if len(coords) != 3 {
					t.Fatalf("expected 3 coordinates, got %d", len(coords))
				}
				// Mapbox format: [lng, lat]
				if coords[0][0] != coords[0][0] { // sanity: not NaN
					t.Fatal("coordinate is NaN")
				}
				// First point: lat=38.5, lng=-120.2 -> [lng, lat] = [-120.2, 38.5]
				wantLng, wantLat := -120.2, 38.5
				if !floatClose(coords[0][0], wantLng) || !floatClose(coords[0][1], wantLat) {
					t.Fatalf("first coord = [%f, %f], want [%f, %f]",
						coords[0][0], coords[0][1], wantLng, wantLat)
				}
			},
		},
		{
			name: "empty routeLine is skipped",
			fields: map[string]events.TelemetryValue{
				"routeLine": {StringVal: ptrString("")},
			},
			wantKeys: nil,
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				if _, ok := result["routeCoordinates"]; ok {
					t.Fatal("expected no routeCoordinates for empty routeLine")
				}
			},
		},
		{
			name: "routeLastUpdated passes through",
			fields: map[string]events.TelemetryValue{
				"routeLastUpdated": {StringVal: ptrString("2026-03-20T12:00:00Z")},
			},
			wantKeys: []string{"routeLastUpdated"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				got, ok := result["routeLastUpdated"].(string)
				if !ok {
					t.Fatalf("expected routeLastUpdated to be string, got %T", result["routeLastUpdated"])
				}
				if got != "2026-03-20T12:00:00Z" {
					t.Fatalf("expected routeLastUpdated=2026-03-20T12:00:00Z, got %q", got)
				}
			},
		},
		{
			name: "nil location is skipped",
			fields: map[string]events.TelemetryValue{
				"location": {}, // no LocationVal set
			},
			wantKeys: nil,
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				if _, ok := result["latitude"]; ok {
					t.Fatal("expected no latitude key for nil location")
				}
			},
		},
		{
			name: "empty TelemetryValue is skipped",
			fields: map[string]events.TelemetryValue{
				"speed": {}, // all nil
			},
			wantKeys: nil,
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				if _, ok := result["speed"]; ok {
					t.Fatal("expected no speed key for empty value")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapFieldsForClient(tt.fields)
			for _, key := range tt.wantKeys {
				if _, ok := result[key]; !ok {
					t.Fatalf("expected key %q in result, got %v", key, result)
				}
			}
			tt.check(t, result)
		})
	}
}

func TestFieldMapping_AllNineFields(t *testing.T) {
	fields := map[string]events.TelemetryValue{
		"speed":          {FloatVal: ptrFloat64(65.0)},
		"location":       {LocationVal: &events.Location{Latitude: 37.0, Longitude: -122.0}},
		"heading":        {FloatVal: ptrFloat64(90.0)},
		"gear":           {StringVal: ptrString("D")},
		"soc":            {IntVal: ptrInt64(80)},
		"estimatedRange": {FloatVal: ptrFloat64(200.0)},
		"insideTemp":     {FloatVal: ptrFloat64(70.0)},
		"outsideTemp":    {FloatVal: ptrFloat64(55.0)},
		"odometer":       {FloatVal: ptrFloat64(10000.0)},
	}

	result := mapFieldsForClient(fields)

	// location expands to latitude+longitude, so 9 input fields → 10 output keys.
	wantKeys := []string{
		"speed", "latitude", "longitude", "heading", "gearPosition",
		"chargeLevel", "estimatedRange", "interiorTemp", "exteriorTemp",
		"odometerMiles",
	}
	for _, key := range wantKeys {
		if _, ok := result[key]; !ok {
			t.Errorf("missing key %q in result: %v", key, result)
		}
	}
	if len(result) != len(wantKeys) {
		t.Errorf("expected %d keys, got %d: %v", len(wantKeys), len(result), result)
	}
}

func TestMarshalWSMessage(t *testing.T) {
	tests := []struct {
		name    string
		msgType string
		payload any
		wantErr bool
	}{
		{
			name:    "vehicle_update",
			msgType: msgTypeVehicleUpdate,
			payload: vehicleUpdatePayload{
				VehicleID: "v-1",
				Fields:    map[string]any{"speed": 65},
				Timestamp: "2026-03-18T12:00:00Z",
			},
		},
		{
			name:    "drive_started",
			msgType: msgTypeDriveStarted,
			payload: driveStartedPayload{
				VehicleID: "v-1",
				DriveID:   "d-1",
				StartLocation: startLocation{
					Latitude:  37.7749,
					Longitude: -122.4194,
				},
				Timestamp: "2026-03-18T12:00:00Z",
			},
		},
		{
			name:    "drive_ended",
			msgType: msgTypeDriveEnded,
			payload: driveEndedPayload{
				VehicleID: "v-1",
				DriveID:   "d-1",
				Distance:  15.3,
				Duration:  "25m0s",
				AvgSpeed:  36.7,
				MaxSpeed:  65.0,
				Timestamp: "2026-03-18T12:30:00Z",
			},
		},
		{
			name:    "connectivity",
			msgType: msgTypeConnectivity,
			payload: connectivityPayload{
				VehicleID: "v-1",
				Online:    true,
				Timestamp: "2026-03-18T12:00:00Z",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := marshalWSMessage(tt.msgType, tt.payload)
			if (err != nil) != tt.wantErr {
				t.Fatalf("marshalWSMessage() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			var msg wsMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			if msg.Type != tt.msgType {
				t.Fatalf("expected type %q, got %q", tt.msgType, msg.Type)
			}
			if msg.Payload == nil {
				t.Fatal("expected non-nil payload")
			}
		})
	}
}

func TestMarshalWSMessage_VehicleUpdate_FieldValues(t *testing.T) {
	raw, err := marshalWSMessage(msgTypeVehicleUpdate, vehicleUpdatePayload{
		VehicleID: "v-1",
		Fields: map[string]any{
			"speed":     65.5,
			"latitude":  37.7749,
			"longitude": -122.4194,
		},
		Timestamp: "2026-03-18T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshalWSMessage: %v", err)
	}

	var msg wsMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	var payload vehicleUpdatePayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if payload.VehicleID != "v-1" {
		t.Fatalf("expected vehicleId=v-1, got %q", payload.VehicleID)
	}
	if payload.Timestamp != "2026-03-18T12:00:00Z" {
		t.Fatalf("expected timestamp=2026-03-18T12:00:00Z, got %q", payload.Timestamp)
	}

	// JSON numbers are float64.
	if speed, ok := payload.Fields["speed"].(float64); !ok || speed != 65.5 {
		t.Fatalf("expected speed=65.5, got %v", payload.Fields["speed"])
	}
}

func TestMarshalWSMessage_DriveStarted_Payload(t *testing.T) {
	raw, err := marshalWSMessage(msgTypeDriveStarted, driveStartedPayload{
		VehicleID: "v-1",
		DriveID:   "drive-abc",
		StartLocation: startLocation{
			Latitude:  37.7749,
			Longitude: -122.4194,
		},
		Timestamp: "2026-03-18T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshalWSMessage: %v", err)
	}

	var msg wsMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	var payload driveStartedPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if payload.VehicleID != "v-1" {
		t.Fatalf("expected vehicleId=v-1, got %q", payload.VehicleID)
	}
	if payload.DriveID != "drive-abc" {
		t.Fatalf("expected driveId=drive-abc, got %q", payload.DriveID)
	}
	if payload.StartLocation.Latitude != 37.7749 {
		t.Fatalf("expected latitude=37.7749, got %v", payload.StartLocation.Latitude)
	}
	if payload.StartLocation.Longitude != -122.4194 {
		t.Fatalf("expected longitude=-122.4194, got %v", payload.StartLocation.Longitude)
	}
}

func TestMarshalWSMessage_DriveEnded_Payload(t *testing.T) {
	raw, err := marshalWSMessage(msgTypeDriveEnded, driveEndedPayload{
		VehicleID: "v-1",
		DriveID:   "drive-abc",
		Distance:  15.3,
		Duration:  "25m0s",
		AvgSpeed:  36.7,
		MaxSpeed:  65.0,
		Timestamp: "2026-03-18T12:30:00Z",
	})
	if err != nil {
		t.Fatalf("marshalWSMessage: %v", err)
	}

	var msg wsMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	var payload driveEndedPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if payload.Distance != 15.3 {
		t.Fatalf("expected distance=15.3, got %v", payload.Distance)
	}
	if payload.Duration != "25m0s" {
		t.Fatalf("expected duration=25m0s, got %q", payload.Duration)
	}
	if payload.AvgSpeed != 36.7 {
		t.Fatalf("expected avgSpeed=36.7, got %v", payload.AvgSpeed)
	}
	if payload.MaxSpeed != 65.0 {
		t.Fatalf("expected maxSpeed=65.0, got %v", payload.MaxSpeed)
	}
}

func TestMarshalWSMessage_Connectivity_Payload(t *testing.T) {
	tests := []struct {
		name   string
		online bool
	}{
		{"connected", true},
		{"disconnected", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := marshalWSMessage(msgTypeConnectivity, connectivityPayload{
				VehicleID: "v-1",
				Online:    tt.online,
				Timestamp: "2026-03-18T12:00:00Z",
			})
			if err != nil {
				t.Fatalf("marshalWSMessage: %v", err)
			}

			var msg wsMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}

			var payload connectivityPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}

			if payload.Online != tt.online {
				t.Fatalf("expected online=%v, got %v", tt.online, payload.Online)
			}
		})
	}
}

func TestDeriveVehicleStatus(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"gear D speed 0", map[string]any{"gearPosition": "D", "speed": 0.0}, "driving"},
		{"gear R speed 0", map[string]any{"gearPosition": "R", "speed": 0.0}, "driving"},
		{"gear P speed 0", map[string]any{"gearPosition": "P", "speed": 0.0}, "parked"},
		{"no gear speed 65 float", map[string]any{"speed": 65.0}, "driving"},
		{"no gear speed 65 int", map[string]any{"speed": 65}, "driving"},
		{"gear N speed 0", map[string]any{"gearPosition": "N", "speed": 0.0}, "parked"},
		{"empty fields", map[string]any{}, "parked"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveVehicleStatus(tt.fields)
			if got != tt.want {
				t.Fatalf("deriveVehicleStatus(%v) = %q, want %q", tt.fields, got, tt.want)
			}
		})
	}
}

func TestTranslateFieldName(t *testing.T) {
	tests := []struct {
		internal string
		want     string
	}{
		{"speed", "speed"},
		{"heading", "heading"},
		{"estimatedRange", "estimatedRange"},
		{"soc", "chargeLevel"},
		{"gear", "gearPosition"},
		{"odometer", "odometerMiles"},
		{"insideTemp", "interiorTemp"},
		{"outsideTemp", "exteriorTemp"},
		{"minutesToArrival", "etaMinutes"},
		{"fsdMilesSinceReset", "fsdMilesToday"},
		{"milesToArrival", "tripDistanceRemaining"},
		{"unknownField", "unknownField"}, // passthrough
	}

	for _, tt := range tests {
		t.Run(tt.internal+"->"+tt.want, func(t *testing.T) {
			got := translateFieldName(tt.internal)
			if got != tt.want {
				t.Fatalf("translateFieldName(%q) = %q, want %q", tt.internal, got, tt.want)
			}
		})
	}
}

func TestUnwrapValue(t *testing.T) {
	tests := []struct {
		name string
		val  events.TelemetryValue
		want any
	}{
		{"float", events.TelemetryValue{FloatVal: ptrFloat64(42.5)}, 42.5},
		{"int", events.TelemetryValue{IntVal: ptrInt64(100)}, int64(100)},
		{"string", events.TelemetryValue{StringVal: ptrString("D")}, "D"},
		{"bool", events.TelemetryValue{BoolVal: ptrBool(true)}, true},
		{"location returns nil", events.TelemetryValue{LocationVal: &events.Location{}}, nil},
		{"empty returns nil", events.TelemetryValue{}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unwrapValue(tt.val)
			if got != tt.want {
				t.Fatalf("unwrapValue() = %v (%T), want %v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
}

// assertFloat asserts a map value is a float64 equal to the expected value.
func assertFloat(t *testing.T, m map[string]any, key string, want float64) {
	t.Helper()
	got, ok := m[key].(float64)
	if !ok {
		t.Fatalf("expected %s to be float64, got %T (%v)", key, m[key], m[key])
	}
	if got != want {
		t.Fatalf("expected %s=%v, got %v", key, want, got)
	}
}

// assertInt asserts a map value is an int equal to the expected value.
func assertInt(t *testing.T, m map[string]any, key string, want int) {
	t.Helper()
	got, ok := m[key].(int)
	if !ok {
		t.Fatalf("expected %s to be int, got %T (%v)", key, m[key], m[key])
	}
	if got != want {
		t.Fatalf("expected %s=%v, got %v", key, want, got)
	}
}

// waitForCondition polls until fn returns true or times out.
func waitForCondition(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for condition")
		case <-tick.C:
		}
	}
}

// floatClose returns true if a and b are within 1e-4 of each other.
func floatClose(a, b float64) bool {
	return math.Abs(a-b) < 1e-4
}
