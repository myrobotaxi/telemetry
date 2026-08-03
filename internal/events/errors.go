package events

import (
	"errors"
	"fmt"
)

var (
	// ErrBusClosed is returned when Publish or Subscribe is called on a
	// bus that has been shut down via Close.
	ErrBusClosed = errors.New("event bus is closed")

	// ErrSubscriptionNotFound is returned by Unsubscribe when the given
	// subscription ID does not match any active subscription.
	ErrSubscriptionNotFound = errors.New("subscription not found")

	// ErrDrainIncomplete is returned by Close when the drain deadline expired
	// with work outstanding. Match it with errors.Is; the concrete
	// *DrainIncompleteError carries the counts.
	ErrDrainIncomplete = errors.New("event bus drain incomplete")
)

// DrainIncompleteError says how much a timed-out Close abandoned (MYR-410).
//
// Close used to return nil on a drain timeout, which made a partial shutdown
// indistinguishable from a clean one: the caller went on to tear down the
// database while handlers were still running against it, and nothing anywhere
// recorded that events had been dropped. A deploy that loses a rider's push
// should leave a number behind.
type DrainIncompleteError struct {
	// HandlersLive is the number of subscriber delivery goroutines still
	// running when Close gave up. They are NOT stopped — they keep running
	// against whatever the caller tears down next.
	HandlersLive int
	// EventsUndrained is the number of accepted events still sitting in
	// subscriber buffers, never delivered to a handler.
	EventsUndrained int
}

func (e *DrainIncompleteError) Error() string {
	return fmt.Sprintf("%s: %d handler(s) still running, %d event(s) undelivered",
		ErrDrainIncomplete.Error(), e.HandlersLive, e.EventsUndrained)
}

func (e *DrainIncompleteError) Unwrap() error { return ErrDrainIncomplete }
