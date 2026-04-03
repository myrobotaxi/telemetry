package ws

import (
	"sync"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

func TestIsNavField(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"routeLine", true},
		{"destinationName", true},
		{"minutesToArrival", true},
		{"milesToArrival", true},
		{"destinationLocation", true},
		{"originLocation", true},
		{"speed", false},
		{"location", false},
		{"gear", false},
		{"soc", false},
		{"heading", false},
		{"estimatedRange", false},
		{"hvacPower", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNavField(tt.name)
			if got != tt.want {
				t.Fatalf("isNavField(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestNavAccumulator_SingleFieldFlushesAfterInterval(t *testing.T) {
	var mu sync.Mutex
	var flushed map[string]events.TelemetryValue
	var flushedVIN string

	acc := newNavAccumulator(50*time.Millisecond, func(vin string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushedVIN = vin
		flushed = fields
	})

	vin := "5YJ3E1EA1NF000001"
	acc.Add(vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Home")},
	})

	// Wait for timer to fire.
	waitForCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return flushed != nil
	})

	mu.Lock()
	defer mu.Unlock()

	if flushedVIN != vin {
		t.Fatalf("expected VIN %q, got %q", vin, flushedVIN)
	}
	if len(flushed) != 1 {
		t.Fatalf("expected 1 field, got %d", len(flushed))
	}
	if *flushed["destinationName"].StringVal != "Home" {
		t.Fatalf("expected destinationName=Home, got %v", flushed["destinationName"])
	}
}

func TestNavAccumulator_MultipleFieldsMergedIntoOneFlush(t *testing.T) {
	var mu sync.Mutex
	var flushed map[string]events.TelemetryValue
	flushCount := 0

	acc := newNavAccumulator(100*time.Millisecond, func(_ string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushed = fields
		flushCount++
	})

	vin := "5YJ3E1EA1NF000001"

	// Add fields in two separate calls within the window.
	acc.Add(vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Work")},
	})
	acc.Add(vin, map[string]events.TelemetryValue{
		"minutesToArrival": {FloatVal: ptrFloat64(15)},
		"milesToArrival":   {FloatVal: ptrFloat64(8.5)},
	})

	waitForCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return flushed != nil
	})

	mu.Lock()
	defer mu.Unlock()

	if flushCount != 1 {
		t.Fatalf("expected 1 flush, got %d", flushCount)
	}
	if len(flushed) != 3 {
		t.Fatalf("expected 3 fields, got %d: %v", len(flushed), flushed)
	}
	if *flushed["destinationName"].StringVal != "Work" {
		t.Fatalf("expected destinationName=Work, got %v", flushed["destinationName"])
	}
	if *flushed["minutesToArrival"].FloatVal != 15 {
		t.Fatalf("expected minutesToArrival=15, got %v", flushed["minutesToArrival"])
	}
}

func TestNavAccumulator_LastWriteWins(t *testing.T) {
	var mu sync.Mutex
	var flushed map[string]events.TelemetryValue

	acc := newNavAccumulator(100*time.Millisecond, func(_ string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushed = fields
	})

	vin := "5YJ3E1EA1NF000001"

	acc.Add(vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Old Place")},
	})
	acc.Add(vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("New Place")},
	})

	waitForCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return flushed != nil
	})

	mu.Lock()
	defer mu.Unlock()

	if *flushed["destinationName"].StringVal != "New Place" {
		t.Fatalf("expected last-write-wins destinationName=New Place, got %v",
			*flushed["destinationName"].StringVal)
	}
}

func TestNavAccumulator_InvalidFieldsAccumulated(t *testing.T) {
	var mu sync.Mutex
	var flushed map[string]events.TelemetryValue

	acc := newNavAccumulator(50*time.Millisecond, func(_ string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushed = fields
	})

	vin := "5YJ3E1EA1NF000001"
	acc.Add(vin, map[string]events.TelemetryValue{
		"destinationName": {Invalid: true},
		"routeLine":       {Invalid: true},
	})

	waitForCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return flushed != nil
	})

	mu.Lock()
	defer mu.Unlock()

	if len(flushed) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(flushed))
	}
	if !flushed["destinationName"].Invalid {
		t.Fatal("expected destinationName.Invalid=true")
	}
	if !flushed["routeLine"].Invalid {
		t.Fatal("expected routeLine.Invalid=true")
	}
}

func TestNavAccumulator_FlushForceReturns(t *testing.T) {
	flushCount := 0
	acc := newNavAccumulator(10*time.Second, func(_ string, _ map[string]events.TelemetryValue) {
		flushCount++
	})

	vin := "5YJ3E1EA1NF000001"
	acc.Add(vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Airport")},
		"milesToArrival":  {FloatVal: ptrFloat64(25)},
	})

	fields := acc.Flush(vin)
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}
	if *fields["destinationName"].StringVal != "Airport" {
		t.Fatalf("expected destinationName=Airport, got %v", fields["destinationName"])
	}

	// Second flush returns nil (already consumed).
	fields = acc.Flush(vin)
	if fields != nil {
		t.Fatalf("expected nil on second flush, got %v", fields)
	}

	// Timer callback should not have fired (was cancelled by Flush).
	if flushCount != 0 {
		t.Fatalf("expected 0 timer flushes, got %d", flushCount)
	}
}

func TestNavAccumulator_FlushEmptyVIN(t *testing.T) {
	acc := newNavAccumulator(500*time.Millisecond, func(_ string, _ map[string]events.TelemetryValue) {
		t.Fatal("onFlush should not be called")
	})

	fields := acc.Flush("nonexistent")
	if fields != nil {
		t.Fatalf("expected nil for unknown VIN, got %v", fields)
	}
}

func TestNavAccumulator_ClearRemovesState(t *testing.T) {
	flushCount := 0
	acc := newNavAccumulator(10*time.Second, func(_ string, _ map[string]events.TelemetryValue) {
		flushCount++
	})

	vin := "5YJ3E1EA1NF000001"
	acc.Add(vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Mall")},
	})

	acc.Clear(vin)

	// Flush after clear returns nil.
	fields := acc.Flush(vin)
	if fields != nil {
		t.Fatal("expected nil after clear")
	}

	// Timer should have been cancelled.
	time.Sleep(50 * time.Millisecond)
	if flushCount != 0 {
		t.Fatalf("expected 0 timer flushes after clear, got %d", flushCount)
	}
}

func TestNavAccumulator_ClearUnknownVIN(t *testing.T) {
	acc := newNavAccumulator(500*time.Millisecond, nil)

	// Should not panic.
	acc.Clear("nonexistent")
}

func TestNavAccumulator_MultipleVINsIndependent(t *testing.T) {
	var mu sync.Mutex
	flushedByVIN := make(map[string]map[string]events.TelemetryValue)

	acc := newNavAccumulator(50*time.Millisecond, func(vin string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushedByVIN[vin] = fields
	})

	vin1 := "5YJ3E1EA1NF000001"
	vin2 := "5YJ3E1EA1NF000002"

	acc.Add(vin1, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Home")},
	})
	acc.Add(vin2, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Work")},
	})

	waitForCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(flushedByVIN) == 2
	})

	mu.Lock()
	defer mu.Unlock()

	if *flushedByVIN[vin1]["destinationName"].StringVal != "Home" {
		t.Fatalf("vin1 expected Home, got %v", flushedByVIN[vin1]["destinationName"])
	}
	if *flushedByVIN[vin2]["destinationName"].StringVal != "Work" {
		t.Fatalf("vin2 expected Work, got %v", flushedByVIN[vin2]["destinationName"])
	}
}

func TestNavAccumulator_TimerFiresCallback(t *testing.T) {
	callbackFired := make(chan struct{}, 1)

	acc := newNavAccumulator(50*time.Millisecond, func(_ string, _ map[string]events.TelemetryValue) {
		callbackFired <- struct{}{}
	})

	acc.Add("5YJ3E1EA1NF000001", map[string]events.TelemetryValue{
		"routeLine": {StringVal: ptrString("encoded-route-data")},
	})

	select {
	case <-callbackFired:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for timer callback")
	}
}
