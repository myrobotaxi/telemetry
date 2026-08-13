package push

import (
	"context"
	"errors"
)

// Notification is one alert bound for one device.
//
// Payload policy (MYR-186): Title/Body may contain a requester FIRST NAME or a
// vehicle NICKNAME and nothing else. Addresses, place labels and coordinates are
// P1 and MUST NEVER appear here — the client refetches ride detail over
// authenticated REST when it needs them.
type Notification struct {
	// DeviceToken is the APNs device token. P1 device identifier — log only a
	// short prefix via tokenPrefix, never the whole value.
	DeviceToken string
	// Sandbox selects the APNs sandbox host (development builds).
	Sandbox bool
	Title   string
	Body    string
	// RideID is the sole userInfo key delivered to the app.
	RideID string
	// EventTopic is the DOMAIN event this alert answers to — `ride.due`,
	// `ride.status.changed`, … It is NOT the APNs topic (the app's bundle id),
	// which the Client holds and which never varies per notification.
	//
	// It exists for one purpose: with RideID it forms the `apns-collapse-id`
	// that makes the transport's one retry idempotent at Apple (MYR-554, see
	// apns_collapse.go). Empty is harmless — the header is then keyed on the
	// ride alone.
	EventTopic string
}

// Sender delivers a single notification. Implementations are best-effort: a
// returned error is advisory and never propagates into the ride flow.
//
// Defined at the consumer site per the project's interface rules; the concrete
// APNs implementation is *Client and the test double is *FakeSender.
type Sender interface {
	Send(ctx context.Context, n Notification) error
}

// ErrUnregistered means APNs has permanently rejected the device token
// (410 Unregistered, or 400 BadDeviceToken). The caller deletes the device row
// — the sender never touches storage itself.
var ErrUnregistered = errors.New("push: device token unregistered")

// ErrThrottled means APNs returned 429 TooManyRequests for this device. The
// notification is dropped without retry; there is no queue to drain into.
var ErrThrottled = errors.New("push: throttled by APNs")

// tokenPrefix returns the loggable form of a device token: the first 8
// characters. Device tokens are P1 identifiers and are never logged in full.
func tokenPrefix(deviceToken string) string {
	const n = 8
	if len(deviceToken) <= n {
		return deviceToken
	}
	return deviceToken[:n]
}
