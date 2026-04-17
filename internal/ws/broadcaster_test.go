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

	if len(b.subs) != 5 {
		t.Fatalf("expected 5 subscriptions, got %d", len(b.subs))
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
			// Tesla's RouteLine is Base64-encoded protobuf wrapping a Google
			// Encoded Polyline at 1e6 precision. This test uses a real
			// protobuf-wrapped polyline encoding 3 points near 38.5/-120.2.
			name: "routeLine decodes to navRouteCoordinates in lng/lat order",
			fields: map[string]events.TelemetryValue{
				"routeLine": {StringVal: ptrString("CiBfaXpsaEF+cmxnZEZfe2dlQ355d2xAX2t3ekNuYHtuSQ==")},
			},
			wantKeys: []string{"navRouteCoordinates"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				coords, ok := result["navRouteCoordinates"].([][]float64)
				if !ok {
					t.Fatalf("expected navRouteCoordinates to be [][]float64, got %T", result["navRouteCoordinates"])
				}
				if len(coords) != 3 {
					t.Fatalf("expected 3 coordinates, got %d", len(coords))
				}
				// First point: lat=38.5, lng=-120.2 -> Mapbox [lng, lat] = [-120.2, 38.5]
				wantLng, wantLat := -120.2, 38.5
				if !floatClose(coords[0][0], wantLng) || !floatClose(coords[0][1], wantLat) {
					t.Fatalf("first coord = [%f, %f], want [%f, %f]",
						coords[0][0], coords[0][1], wantLng, wantLat)
				}
			},
		},
		{
			name: "empty routeLine clears navRouteCoordinates",
			fields: map[string]events.TelemetryValue{
				"routeLine": {StringVal: ptrString("")},
			},
			wantKeys: []string{"navRouteCoordinates"},
			check: func(t *testing.T, result map[string]any) {
				t.Helper()
				val, ok := result["navRouteCoordinates"]
				if !ok {
					t.Fatal("expected navRouteCoordinates key to be present (as nil)")
				}
				if val != nil {
					t.Fatalf("expected navRouteCoordinates to be nil, got %v", val)
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
				VehicleID:       "v-1",
				DriveID:         "d-1",
				Distance:        15.3,
				DurationSeconds: 1500,
				AvgSpeed:        36.7,
				MaxSpeed:        65.0,
				Timestamp:       "2026-03-18T12:30:00Z",
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
		VehicleID:       "v-1",
		DriveID:         "drive-abc",
		Distance:        15.3,
		DurationSeconds: 1500,
		AvgSpeed:        36.7,
		MaxSpeed:        65.0,
		Timestamp:       "2026-03-18T12:30:00Z",
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
	if payload.DurationSeconds != 1500 {
		t.Fatalf("expected durationSeconds=1500, got %v", payload.DurationSeconds)
	}
	if payload.AvgSpeed != 36.7 {
		t.Fatalf("expected avgSpeed=36.7, got %v", payload.AvgSpeed)
	}
	if payload.MaxSpeed != 65.0 {
		t.Fatalf("expected maxSpeed=65.0, got %v", payload.MaxSpeed)
	}
}

func TestDriveEnded_DurationSeconds_MatchesDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     float64
	}{
		{"25 minutes", 25 * time.Minute, 1500},
		{"1 hour 30 seconds", time.Hour + 30*time.Second, 3630},
		{"zero", 0, 0},
		{"sub-second", 500 * time.Millisecond, 0.5},
		{"24 minutes 18 seconds", 24*time.Minute + 18*time.Second, 1458},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := driveEndedPayload{
				VehicleID:       "v-1",
				DriveID:         "d-1",
				DurationSeconds: tt.duration.Seconds(),
				Timestamp:       "2026-03-18T12:30:00Z",
			}

			raw, err := marshalWSMessage(msgTypeDriveEnded, p)
			if err != nil {
				t.Fatalf("marshalWSMessage: %v", err)
			}

			var msg wsMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}

			var got driveEndedPayload
			if err := json.Unmarshal(msg.Payload, &got); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}

			if math.Abs(got.DurationSeconds-tt.want) > 1e-9 {
				t.Fatalf("durationSeconds = %v, want %v", got.DurationSeconds, tt.want)
			}

			// Verify JSON key is "durationSeconds", not "duration".
			var raw2 map[string]json.RawMessage
			if err := json.Unmarshal(msg.Payload, &raw2); err != nil {
				t.Fatalf("unmarshal raw payload: %v", err)
			}
			if _, ok := raw2["durationSeconds"]; !ok {
				t.Fatal("expected JSON key \"durationSeconds\" in payload")
			}
			if _, ok := raw2["duration"]; ok {
				t.Fatal("unexpected JSON key \"duration\" in payload — must use \"durationSeconds\"")
			}
		})
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
		{"gear D speed 0 (red light)", map[string]any{"gearPosition": "D", "speed": 0.0}, "driving"},
		{"gear D speed 35", map[string]any{"gearPosition": "D", "speed": 35.0}, "driving"},
		{"gear R speed 0", map[string]any{"gearPosition": "R", "speed": 0.0}, "driving"},
		{"gear R speed 5", map[string]any{"gearPosition": "R", "speed": 5.0}, "driving"},
		{"gear P speed 0", map[string]any{"gearPosition": "P", "speed": 0.0}, "parked"},
		{"no gear speed 0", map[string]any{"speed": 0.0}, "parked"},
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
		{"fsdMilesSinceReset", "fsdMilesSinceReset"},
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


func TestBroadcaster_HandleDriveUpdated(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{
		"5YJ3E1EA1NF000001": "vehicle-id-1",
	})

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	// Override accumulator with batch size 2 for faster test feedback.
	b.routes = newRouteAccumulator(2, 0)

	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)

	// Publish first route point — should not trigger a broadcast.
	event1 := events.NewEvent(events.DriveUpdatedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive-abc",
		RoutePoint: events.RoutePoint{
			Latitude:  37.7749,
			Longitude: -122.4194,
			Speed:     35.0,
			Heading:   180.0,
			Timestamp: now,
		},
	})

	if err := bus.Publish(ctx, event1); err != nil {
		t.Fatalf("Publish event1: %v", err)
	}

	// Brief pause to let async handler run.
	time.Sleep(50 * time.Millisecond)

	// Resolver should not have been called yet (batch not full).
	resolver.mu.RLock()
	callsBefore := len(resolver.callLog)
	resolver.mu.RUnlock()

	// Publish second route point — batch size 2 triggers flush + VIN resolution.
	event2 := events.NewEvent(events.DriveUpdatedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive-abc",
		RoutePoint: events.RoutePoint{
			Latitude:  37.7750,
			Longitude: -122.4195,
			Speed:     40.0,
			Heading:   185.0,
			Timestamp: now.Add(time.Second),
		},
	})

	if err := bus.Publish(ctx, event2); err != nil {
		t.Fatalf("Publish event2: %v", err)
	}

	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > callsBefore
	})
}

func TestBroadcaster_DriveEndedClearsAccumulator(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{
		"5YJ3E1EA1NF000001": "vehicle-id-1",
	})

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	// Large batch so points accumulate without flushing.
	b.routes = newRouteAccumulator(100, 0)

	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)

	// Accumulate some route points.
	for i := 0; i < 3; i++ {
		event := events.NewEvent(events.DriveUpdatedEvent{
			VIN:     "5YJ3E1EA1NF000001",
			DriveID: "drive-abc",
			RoutePoint: events.RoutePoint{
				Latitude:  37.7749 + float64(i)*0.001,
				Longitude: -122.4194 + float64(i)*0.001,
				Speed:     35.0,
				Heading:   180.0,
				Timestamp: now.Add(time.Duration(i) * time.Second),
			},
		})
		if err := bus.Publish(ctx, event); err != nil {
			t.Fatalf("Publish route point %d: %v", i, err)
		}
	}

	// Wait for all 3 route points to be added to the accumulator.
	// handleDriveUpdated is silent until the batch flushes (size 100 here),
	// so we poll the accumulator's length directly instead of inferring
	// from resolver calls. Polling instead of time.Sleep per CLAUDE.md
	// "no sleep in tests" rule.
	waitForCondition(t, func() bool {
		return b.routes.Len("5YJ3E1EA1NF000001") >= 3
	})

	// End the drive — flushes remaining points and clears accumulator.
	endEvent := events.NewEvent(events.DriveEndedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive-abc",
		Stats: events.DriveStats{
			Distance: 5.0,
			Duration: 10 * time.Minute,
			AvgSpeed: 30.0,
			MaxSpeed: 45.0,
		},
		EndedAt: now.Add(10 * time.Minute),
	})

	if err := bus.Publish(ctx, endEvent); err != nil {
		t.Fatalf("Publish drive ended: %v", err)
	}

	// Wait for handleDriveEnded to clear the accumulator. The previous
	// wait condition polled the resolver call log, but the resolver is
	// called BEFORE the accumulator is cleared in handleDriveEnded,
	// so the old condition could be satisfied while routes.Clear had
	// not yet executed — a race. Polling Len directly asserts the exact
	// invariant this test verifies.
	waitForCondition(t, func() bool {
		return b.routes.Len("5YJ3E1EA1NF000001") == 0
	})

	// Accumulator should be cleared for this VIN.
	remaining := b.routes.Flush("5YJ3E1EA1NF000001")
	if remaining != nil {
		t.Fatalf("expected nil after drive ended, got %d points", len(remaining))
	}
}

func TestBroadcaster_NavOnlyEvent_NotBroadcastImmediately(t *testing.T) {
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
			"destinationName":  {StringVal: ptrString("Home")},
			"minutesToArrival": {FloatVal: ptrFloat64(15)},
		},
	})

	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Give subscriber time to process event.
	time.Sleep(50 * time.Millisecond)

	// Nav-only event should NOT trigger immediate VIN resolution
	// (nav fields are accumulated, not broadcast immediately).
	resolver.mu.RLock()
	calls := len(resolver.callLog)
	resolver.mu.RUnlock()
	if calls != 0 {
		t.Fatalf("expected 0 resolver calls for nav-only event, got %d", calls)
	}

	// After the nav flush interval, the accumulator should flush.
	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > 0
	})
}

func TestBroadcaster_NonNavEvent_BroadcastImmediately(t *testing.T) {
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
			"speed": {FloatVal: ptrFloat64(65)},
			"gear":  {StringVal: ptrString("D")},
		},
	})

	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Non-nav fields should trigger immediate VIN resolution.
	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > 0
	})

	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	if resolver.callLog[0] != "5YJ3E1EA1NF000001" {
		t.Fatalf("expected VIN 5YJ3E1EA1NF000001, got %q", resolver.callLog[0])
	}
}

func TestBroadcaster_MixedEvent_NonNavImmediateNavAccumulated(t *testing.T) {
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
			"speed":           {FloatVal: ptrFloat64(65)},
			"destinationName": {StringVal: ptrString("Work")},
		},
	})

	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Non-nav field (speed) triggers immediate VIN resolution.
	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > 0
	})

	// First resolver call is for the immediate non-nav broadcast.
	resolver.mu.RLock()
	firstCalls := len(resolver.callLog)
	resolver.mu.RUnlock()
	if firstCalls != 1 {
		t.Fatalf("expected 1 resolver call for non-nav, got %d", firstCalls)
	}

	// Nav fields are accumulated — a second resolver call happens
	// after the nav flush interval.
	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > 1
	})
}

func TestBroadcaster_DriveEndedClearsNavAccumulator(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{
		"5YJ3E1EA1NF000001": "vehicle-id-1",
	})

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	// Use a long flush interval so nav fields stay pending.
	b.nav = newNavAccumulator(10*time.Second, b.flushNav)

	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)

	// Accumulate nav fields via telemetry event.
	telEvent := events.NewEvent(events.VehicleTelemetryEvent{
		VIN:       "5YJ3E1EA1NF000001",
		CreatedAt: now,
		Fields: map[string]events.TelemetryValue{
			"destinationName": {StringVal: ptrString("Airport")},
		},
	})
	if err := bus.Publish(ctx, telEvent); err != nil {
		t.Fatalf("Publish telemetry: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// End drive — should flush and clear nav accumulator.
	endEvent := events.NewEvent(events.DriveEndedEvent{
		VIN:     "5YJ3E1EA1NF000001",
		DriveID: "drive-abc",
		Stats: events.DriveStats{
			Distance: 5.0,
			Duration: 10 * time.Minute,
			AvgSpeed: 30.0,
			MaxSpeed: 45.0,
		},
		EndedAt: now.Add(10 * time.Minute),
	})
	if err := bus.Publish(ctx, endEvent); err != nil {
		t.Fatalf("Publish drive ended: %v", err)
	}

	waitForCondition(t, func() bool {
		resolver.mu.RLock()
		defer resolver.mu.RUnlock()
		return len(resolver.callLog) > 0
	})

	// Nav accumulator should be cleared.
	remaining := b.nav.Flush("5YJ3E1EA1NF000001")
	if remaining != nil {
		t.Fatalf("expected nil nav fields after drive ended, got %v", remaining)
	}
}

func TestStatusDerivation_SpeedOnlyDoesNotInjectStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		fields     map[string]events.TelemetryValue
		wantStatus bool // whether "status" key should exist
		wantValue  string
	}{
		{
			name: "speed 0 without gear does not inject status",
			fields: map[string]events.TelemetryValue{
				"speed": {FloatVal: ptrFloat64(0)},
			},
			wantStatus: false,
		},
		{
			name: "speed 65 without gear does not inject status",
			fields: map[string]events.TelemetryValue{
				"speed": {FloatVal: ptrFloat64(65)},
			},
			wantStatus: false,
		},
		{
			name: "gear D with speed 0 injects driving",
			fields: map[string]events.TelemetryValue{
				"gear":  {StringVal: ptrString("D")},
				"speed": {FloatVal: ptrFloat64(0)},
			},
			wantStatus: true,
			wantValue:  "driving",
		},
		{
			name: "gear P alone injects parked",
			fields: map[string]events.TelemetryValue{
				"gear": {StringVal: ptrString("P")},
			},
			wantStatus: true,
			wantValue:  "parked",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clientFields := mapFieldsForClient(tt.fields)

			// Simulate what handleTelemetry does.
			if _, hasGear := clientFields["gearPosition"]; hasGear {
				clientFields["status"] = deriveVehicleStatus(clientFields)
			}

			_, gotStatus := clientFields["status"]
			if gotStatus != tt.wantStatus {
				t.Fatalf("status present = %v, want %v (fields: %v)", gotStatus, tt.wantStatus, clientFields)
			}
			if tt.wantStatus {
				if clientFields["status"] != tt.wantValue {
					t.Fatalf("status = %q, want %q", clientFields["status"], tt.wantValue)
				}
			}
		})
	}
}

func TestMapFieldsForClient_InvalidNavFieldsClear(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		fields      map[string]events.TelemetryValue
		wantNilKeys []string
		wantAbsent  []string
	}{
		{
			name: "invalid destinationName clears to nil",
			fields: map[string]events.TelemetryValue{
				"destinationName": {Invalid: true},
			},
			wantNilKeys: []string{"destinationName"},
		},
		{
			name: "invalid milesToArrival clears tripDistanceRemaining",
			fields: map[string]events.TelemetryValue{
				"milesToArrival": {Invalid: true},
			},
			wantNilKeys: []string{"tripDistanceRemaining"},
		},
		{
			name: "invalid minutesToArrival clears etaMinutes",
			fields: map[string]events.TelemetryValue{
				"minutesToArrival": {Invalid: true},
			},
			wantNilKeys: []string{"etaMinutes"},
		},
		{
			name: "invalid routeLine clears navRouteCoordinates",
			fields: map[string]events.TelemetryValue{
				"routeLine": {Invalid: true},
			},
			wantNilKeys: []string{"navRouteCoordinates"},
		},
		{
			name: "invalid originLocation clears lat and lng",
			fields: map[string]events.TelemetryValue{
				"originLocation": {Invalid: true},
			},
			wantNilKeys: []string{"originLatitude", "originLongitude"},
		},
		{
			name: "invalid destinationLocation clears lat and lng",
			fields: map[string]events.TelemetryValue{
				"destinationLocation": {Invalid: true},
			},
			wantNilKeys: []string{"destinationLatitude", "destinationLongitude"},
		},
		{
			name: "invalid non-nav field is skipped",
			fields: map[string]events.TelemetryValue{
				"speed": {Invalid: true},
			},
			wantAbsent: []string{"speed"},
		},
		{
			name: "mix of valid and invalid fields",
			fields: map[string]events.TelemetryValue{
				"destinationName": {Invalid: true},
				"speed":           {FloatVal: ptrFloat64(65)},
			},
			wantNilKeys: []string{"destinationName"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := mapFieldsForClient(tt.fields)

			for _, key := range tt.wantNilKeys {
				val, exists := out[key]
				if !exists {
					t.Fatalf("expected key %q to be present (nil), but it was absent", key)
				}
				if val != nil {
					t.Fatalf("expected %q = nil, got %v (%T)", key, val, val)
				}
			}
			for _, key := range tt.wantAbsent {
				if _, exists := out[key]; exists {
					t.Fatalf("expected key %q to be absent, but it was present", key)
				}
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
