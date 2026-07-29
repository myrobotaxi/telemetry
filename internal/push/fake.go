package push

import (
	"context"
	"sync"
)

// FakeSender is the in-memory Sender test double. Every consumer test runs
// against it — no test ever reaches Apple.
type FakeSender struct {
	mu   sync.Mutex
	sent []Notification

	// Err, when non-nil, is returned by every Send (fire-and-forget tests).
	Err error
	// ErrByToken overrides Err for a specific device token, so a test can fail
	// exactly one device (e.g. ErrUnregistered) while the rest succeed.
	ErrByToken map[string]error
}

// NewFakeSender returns a ready FakeSender.
func NewFakeSender() *FakeSender {
	return &FakeSender{ErrByToken: map[string]error{}}
}

// Send records the notification and returns the configured error, if any.
func (f *FakeSender) Send(_ context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, n)
	if err, ok := f.ErrByToken[n.DeviceToken]; ok {
		return err
	}
	return f.Err
}

// Sent returns a copy of everything sent so far.
func (f *FakeSender) Sent() []Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Notification, len(f.sent))
	copy(out, f.sent)
	return out
}
