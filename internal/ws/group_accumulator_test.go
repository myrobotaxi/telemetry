package ws

import (
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
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

func TestGroupAccumulator_SingleFieldFlushesAfterInterval(t *testing.T) {
	var mu sync.Mutex
	var flushed map[string]events.TelemetryValue
	var flushedVIN string
	var flushedGroup atomicGroupID

	acc := newGroupAccumulator(50*time.Millisecond, func(group atomicGroupID, vin string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushedGroup = group
		flushedVIN = vin
		flushed = fields
	})

	vin := "5YJ3E1EA1NF000001"
	acc.Add(groupNavigation, vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Home")},
	})

	waitForCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return flushed != nil
	})

	mu.Lock()
	defer mu.Unlock()

	if flushedGroup != groupNavigation {
		t.Fatalf("expected group %q, got %q", groupNavigation, flushedGroup)
	}
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

func TestGroupAccumulator_MultipleFieldsMergedIntoOneFlush(t *testing.T) {
	var mu sync.Mutex
	var flushed map[string]events.TelemetryValue
	flushCount := 0

	acc := newGroupAccumulator(100*time.Millisecond, func(_ atomicGroupID, _ string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushed = fields
		flushCount++
	})

	vin := "5YJ3E1EA1NF000001"

	acc.Add(groupNavigation, vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Work")},
	})
	acc.Add(groupNavigation, vin, map[string]events.TelemetryValue{
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

func TestGroupAccumulator_LastWriteWins(t *testing.T) {
	var mu sync.Mutex
	var flushed map[string]events.TelemetryValue

	acc := newGroupAccumulator(100*time.Millisecond, func(_ atomicGroupID, _ string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushed = fields
	})

	vin := "5YJ3E1EA1NF000001"

	acc.Add(groupNavigation, vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Old Place")},
	})
	acc.Add(groupNavigation, vin, map[string]events.TelemetryValue{
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

func TestGroupAccumulator_InvalidFieldsAccumulated(t *testing.T) {
	var mu sync.Mutex
	var flushed map[string]events.TelemetryValue

	acc := newGroupAccumulator(50*time.Millisecond, func(_ atomicGroupID, _ string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushed = fields
	})

	vin := "5YJ3E1EA1NF000001"
	acc.Add(groupNavigation, vin, map[string]events.TelemetryValue{
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

func TestGroupAccumulator_FlushForceReturns(t *testing.T) {
	flushCount := 0
	acc := newGroupAccumulator(10*time.Second, func(_ atomicGroupID, _ string, _ map[string]events.TelemetryValue) {
		flushCount++
	})

	vin := "5YJ3E1EA1NF000001"
	acc.Add(groupNavigation, vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Airport")},
		"milesToArrival":  {FloatVal: ptrFloat64(25)},
	})

	fields := acc.Flush(groupNavigation, vin)
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}
	if *fields["destinationName"].StringVal != "Airport" {
		t.Fatalf("expected destinationName=Airport, got %v", fields["destinationName"])
	}

	// Second flush returns nil (already consumed).
	fields = acc.Flush(groupNavigation, vin)
	if fields != nil {
		t.Fatalf("expected nil on second flush, got %v", fields)
	}

	// Timer callback should not have fired (was cancelled by Flush).
	if flushCount != 0 {
		t.Fatalf("expected 0 timer flushes, got %d", flushCount)
	}
}

func TestGroupAccumulator_FlushEmptyVIN(t *testing.T) {
	acc := newGroupAccumulator(500*time.Millisecond, func(_ atomicGroupID, _ string, _ map[string]events.TelemetryValue) {
		t.Fatal("onFlush should not be called")
	})

	fields := acc.Flush(groupNavigation, "nonexistent")
	if fields != nil {
		t.Fatalf("expected nil for unknown VIN, got %v", fields)
	}
}

func TestGroupAccumulator_ClearRemovesState(t *testing.T) {
	// Use a short interval so we can verify the timer was cancelled by
	// waiting for it NOT to fire within a bounded duration.
	callbackFired := make(chan struct{}, 1)
	acc := newGroupAccumulator(50*time.Millisecond, func(_ atomicGroupID, _ string, _ map[string]events.TelemetryValue) {
		callbackFired <- struct{}{}
	})

	vin := "5YJ3E1EA1NF000001"
	acc.Add(groupNavigation, vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Mall")},
	})

	acc.Clear(groupNavigation, vin)

	// Flush after clear returns nil.
	fields := acc.Flush(groupNavigation, vin)
	if fields != nil {
		t.Fatal("expected nil after clear")
	}

	// Timer should have been cancelled — verify no callback within 3x the interval.
	select {
	case <-callbackFired:
		t.Fatal("expected no timer flush after clear, but callback fired")
	case <-time.After(150 * time.Millisecond):
		// Success — timer did not fire.
	}
}

func TestGroupAccumulator_ClearUnknownVIN(t *testing.T) {
	acc := newGroupAccumulator(500*time.Millisecond, nil)

	// Should not panic.
	acc.Clear(groupNavigation, "nonexistent")
}

func TestGroupAccumulator_MultipleVINsIndependent(t *testing.T) {
	var mu sync.Mutex
	flushedByVIN := make(map[string]map[string]events.TelemetryValue)

	acc := newGroupAccumulator(50*time.Millisecond, func(_ atomicGroupID, vin string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushedByVIN[vin] = fields
	})

	vin1 := "5YJ3E1EA1NF000001"
	vin2 := "5YJ3E1EA1NF000002"

	acc.Add(groupNavigation, vin1, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Home")},
	})
	acc.Add(groupNavigation, vin2, map[string]events.TelemetryValue{
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

func TestGroupAccumulator_StopCancelsAllTimers(t *testing.T) {
	callbackFired := make(chan struct{}, 2)
	acc := newGroupAccumulator(50*time.Millisecond, func(_ atomicGroupID, _ string, _ map[string]events.TelemetryValue) {
		callbackFired <- struct{}{}
	})

	acc.Add(groupNavigation, "VIN1", map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("A")},
	})
	acc.Add(groupNavigation, "VIN2", map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("B")},
	})

	acc.Stop()

	// No callbacks should fire after Stop.
	select {
	case <-callbackFired:
		t.Fatal("expected no callbacks after Stop")
	case <-time.After(150 * time.Millisecond):
		// Success.
	}
}

func TestGroupAccumulator_TimerFiresCallback(t *testing.T) {
	callbackFired := make(chan struct{}, 1)

	acc := newGroupAccumulator(50*time.Millisecond, func(_ atomicGroupID, _ string, _ map[string]events.TelemetryValue) {
		callbackFired <- struct{}{}
	})

	acc.Add(groupNavigation, "5YJ3E1EA1NF000001", map[string]events.TelemetryValue{
		"routeLine": {StringVal: ptrString("encoded-route-data")},
	})

	select {
	case <-callbackFired:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for timer callback")
	}
}

// TestGroupAccumulator_DifferentGroupsIndependent verifies that two groups
// for the same VIN accumulate into separate batches and flush
// independently. Forward-compatible test for when a second group registers
// an accumulator slot in a future MYR.
func TestGroupAccumulator_DifferentGroupsIndependent(t *testing.T) {
	var mu sync.Mutex
	flushedByGroup := make(map[atomicGroupID]map[string]events.TelemetryValue)

	acc := newGroupAccumulator(50*time.Millisecond, func(group atomicGroupID, _ string, fields map[string]events.TelemetryValue) {
		mu.Lock()
		defer mu.Unlock()
		flushedByGroup[group] = fields
	})

	vin := "5YJ3E1EA1NF000001"

	acc.Add(groupNavigation, vin, map[string]events.TelemetryValue{
		"destinationName": {StringVal: ptrString("Nav-A")},
	})
	// Use a second group for the same VIN to confirm slot isolation.
	acc.Add(groupCharge, vin, map[string]events.TelemetryValue{
		"chargeState": {StringVal: ptrString("Charging")},
	})

	waitForCondition(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(flushedByGroup) == 2
	})

	mu.Lock()
	defer mu.Unlock()

	if got := *flushedByGroup[groupNavigation]["destinationName"].StringVal; got != "Nav-A" {
		t.Fatalf("navigation slot leaked or lost data: got %q", got)
	}
	if got := *flushedByGroup[groupCharge]["chargeState"].StringVal; got != "Charging" {
		t.Fatalf("charge slot leaked or lost data: got %q", got)
	}

	// Cross-group isolation: navigation batch must not contain chargeState
	// and vice versa.
	if _, leaked := flushedByGroup[groupNavigation]["chargeState"]; leaked {
		t.Fatal("chargeState leaked into navigation batch")
	}
	if _, leaked := flushedByGroup[groupCharge]["destinationName"]; leaked {
		t.Fatal("destinationName leaked into charge batch")
	}
}
