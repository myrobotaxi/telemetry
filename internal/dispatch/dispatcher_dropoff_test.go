package dispatch

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// testStartedEvent is the leg-2 (dropoff) analogue of testEvent: a ride.started
// carrying a DROPOFF place distinct from the pickup so tests can prove leg 2
// pushes the dropoff, not the pickup.
func testStartedEvent() events.RideStartedEvent {
	return events.RideStartedEvent{
		RideRequestID: "cride123",
		VehicleID:     "cveh456",
		OwnerID:       "cowner789",
		Target:        events.RidePlace{Latitude: 40.7128, Longitude: -74.0060, Label: "City Hall"},
	}
}

// TestProcessDropoff_Success proves a rider start pushes the DROPOFF coordinate
// exactly once and records the outcome on the independent leg-2 columns —
// leaving leg-1 (pickup) tracking untouched.
func TestProcessDropoff_Success(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{dropClaimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	d.processDropoff(context.Background(), testStartedEvent())

	if len(exec.calls) != 1 {
		t.Fatalf("want 1 executor call, got %d", len(exec.calls))
	}
	if got := exec.calls[0]; got.Command != commandNavigationRequest {
		t.Errorf("command = %q, want %q", got.Command, commandNavigationRequest)
	}
	// The pushed value must be the DROPOFF coordinate, not a pickup.
	wantValue := "40.7128,-74.006"
	if got := exec.calls[0].Params["value"]; got != wantValue {
		t.Errorf("value param = %v, want %q (dropoff coord)", got, wantValue)
	}
	if len(st.dropRecorded) != 1 || st.dropRecorded[0].status != OutcomeSent || st.dropRecorded[0].code != "" {
		t.Errorf("dropRecorded = %+v, want one {sent, no code}", st.dropRecorded)
	}
	// Leg 1 columns must be untouched.
	if len(st.recorded) != 0 || st.claimCnt != 0 {
		t.Errorf("leg-1 tracking touched by a dropoff dispatch: recorded=%+v claimCnt=%d", st.recorded, st.claimCnt)
	}
}

// TestProcessDropoff_ExactlyOnce_DuplicateDelivery proves the dropoff nav push
// fires exactly once across duplicate ride.started deliveries — the leg-2 claim
// latch wins only the first time.
func TestProcessDropoff_ExactlyOnce_DuplicateDelivery(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{dropClaimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	d.processDropoff(context.Background(), testStartedEvent())
	d.processDropoff(context.Background(), testStartedEvent())

	if len(exec.calls) != 1 {
		t.Errorf("executor calls = %d across duplicate starts, want exactly 1", len(exec.calls))
	}
	if len(st.dropRecorded) != 1 {
		t.Errorf("dropRecorded = %d across duplicate starts, want 1", len(st.dropRecorded))
	}
}

// TestProcessDropoff_Idempotent_AlreadyDispatched proves a lost leg-2 claim
// (already dispatched) pushes nothing and records nothing.
func TestProcessDropoff_Idempotent_AlreadyDispatched(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{dropClaimed: false} // claim loses
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	d.processDropoff(context.Background(), testStartedEvent())

	if len(exec.calls) != 0 || len(st.dropRecorded) != 0 {
		t.Errorf("lost dropoff claim must not dispatch/record: calls=%d recorded=%+v", len(exec.calls), st.dropRecorded)
	}
}

// TestProcessDropoff_KillSwitch proves the DISPATCH_ENABLED kill-switch records
// the dropoff as skipped with no Tesla call — but still latches the claim.
func TestProcessDropoff_KillSwitch(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{dropClaimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: false, MaxRetries: 2})

	d.processDropoff(context.Background(), testStartedEvent())

	if len(exec.calls) != 0 {
		t.Errorf("executor called with kill-switch off, want 0")
	}
	if st.dropClaimCnt != 1 {
		t.Errorf("dropoff claim count = %d, want 1 (skip still latches)", st.dropClaimCnt)
	}
	if len(st.dropRecorded) != 1 || st.dropRecorded[0].status != OutcomeSkipped {
		t.Errorf("dropRecorded = %+v, want one {skipped}", st.dropRecorded)
	}
}

// TestProcessDropoff_OutOfRangeIsTerminal proves an out-of-range dropoff fails
// terminally with invalid_request and never dials Tesla.
func TestProcessDropoff_OutOfRangeIsTerminal(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{dropClaimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	ev := testStartedEvent()
	ev.Target = events.RidePlace{Latitude: 91.0, Longitude: 10.0}
	d.processDropoff(context.Background(), ev)

	if len(exec.calls) != 0 {
		t.Errorf("out-of-range dropoff must not dial Tesla, got %d calls", len(exec.calls))
	}
	if len(st.dropRecorded) != 1 || st.dropRecorded[0].status != OutcomeFailed ||
		st.dropRecorded[0].code != string(wserrors.ErrCodeInvalidRequest) {
		t.Errorf("dropRecorded = %+v, want one {failed, invalid_request}", st.dropRecorded)
	}
}

// TestHandleStarted_WrongPayloadType proves a non-ride.started payload is
// ignored without claiming or panicking.
func TestHandleStarted_WrongPayloadType(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{dropClaimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	d.handleStarted(events.Event{ID: "x", Payload: events.RideStatusChangedEvent{}})
	d.Wait()

	if st.dropClaimCnt != 0 {
		t.Errorf("wrong payload should not claim, got dropClaimCnt=%d", st.dropClaimCnt)
	}
}
