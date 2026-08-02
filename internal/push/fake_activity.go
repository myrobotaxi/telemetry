package push

import (
	"context"
	"sync"
)

// FakeActivitySender is the in-memory ActivitySender double.
//
// A non-test file, like FakeSender beside it, so consumers in other packages
// (the telemetry handlers, the cmd/ wiring tests) can assert on Live Activity
// sends without an APNs key. Real APNs delivery cannot be exercised locally, so
// this is the boundary every test on this surface stops at.
type FakeActivitySender struct {
	mu   sync.Mutex
	sent []ActivityNotification

	// Err is returned by every SendActivity.
	Err error
	// ErrByToken overrides Err for a specific activity token, so a test can
	// make exactly one Activity look unregistered.
	ErrByToken map[string]error

	// Before, when set, runs at the top of every SendActivity — deliberately
	// BEFORE the mutex is taken, so a test may call Sent() from another
	// goroutine while one send is parked inside it.
	//
	// It exists for the shutdown-drain tests (MYR-421), which have to observe
	// the window in which a push is genuinely in flight: the question "does
	// Wait() cover this code path?" cannot be asked of a send that has already
	// returned. Set it before the goroutine that sends is started; the `go`
	// statement is the happens-before edge that keeps the read race-free.
	Before func()
}

// NewFakeActivitySender builds an empty spy.
func NewFakeActivitySender() *FakeActivitySender {
	return &FakeActivitySender{}
}

// SendActivity records the update and returns the configured error, if any.
func (f *FakeActivitySender) SendActivity(_ context.Context, n ActivityNotification) error {
	if f.Before != nil {
		f.Before()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.ErrByToken[n.ActivityToken]; ok {
		// Record the attempt even when it fails: "did we try to push to this
		// token?" is a different question from "did it land", and the
		// unregistered-token tests need both.
		f.sent = append(f.sent, n)
		return err
	}
	f.sent = append(f.sent, n)
	return f.Err
}

// Sent returns a copy of everything recorded so far.
func (f *FakeActivitySender) Sent() []ActivityNotification {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ActivityNotification, len(f.sent))
	copy(out, f.sent)
	return out
}

// Reset drops the recording, so one test can assert across phases.
func (f *FakeActivitySender) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = nil
}
