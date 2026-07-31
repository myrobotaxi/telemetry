package push

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// The ActivityKit half of the APNs client (MYR-172).

// activityAPS is the `aps` dictionary of an ActivityKit remote update.
//
// The key names are Apple's and are hyphenated; they are pinned here as struct
// tags rather than assembled into a map so that a typo is a compile-time
// rename rather than a push Apple answers 400 to. Field order in the struct is
// the order they appear in the rendered JSON, which makes the payload readable
// in a packet capture.
type activityAPS struct {
	// Timestamp is `aps.timestamp` — when the state was true, in unix seconds.
	// ActivityKit drops an update whose timestamp is older than the one it is
	// already showing, which is the only defence against a reordered pair of
	// pushes leaving the lock screen on the stale one.
	Timestamp int64 `json:"timestamp"`

	// Event is `update` or `end`.
	Event ActivityEvent `json:"event"`

	// ContentState is the payload the Swift ContentState decodes.
	ContentState ActivityContentState `json:"content-state"`

	// StaleDate is `aps.stale-date` in unix seconds — past it, ActivityKit
	// renders the Activity as stale on its own. Always set: an update with no
	// stale-date is a promise we cannot keep.
	StaleDate int64 `json:"stale-date"`

	// DismissalDate is `aps.dismissal-date` in unix seconds, present only on an
	// end event. Omitted means iOS dismisses the Activity immediately.
	DismissalDate *int64 `json:"dismissal-date,omitempty"`
}

// activityPayload is the whole APNs body.
//
// Unlike the alert payload beside it, there is NO userInfo: an Activity update
// carries no ride id outside the content-state, because the token itself
// already addresses exactly one Activity for exactly one ride. Adding a ride id
// would be a P0 identifier on the wire buying nothing.
type activityPayload struct {
	APS activityAPS `json:"aps"`
}

// buildActivityPayload renders the APNs JSON body for a Live Activity update.
func buildActivityPayload(n ActivityNotification) ([]byte, error) {
	aps := activityAPS{
		Timestamp:    n.Timestamp.Unix(),
		Event:        n.Event,
		ContentState: n.ContentState,
		StaleDate:    n.StaleDate().Unix(),
	}
	if n.Event == ActivityEventEnd && n.DismissalDate != nil {
		dismiss := n.DismissalDate.Unix()
		aps.DismissalDate = &dismiss
	}

	body, err := json.Marshal(activityPayload{APS: aps})
	if err != nil {
		return nil, fmt.Errorf("push: marshal activity payload: %w", err)
	}
	return body, nil
}

// activityTopic derives the Live Activity topic from the app's bundle id.
//
// Apple requires the `.push-type.liveactivity` suffix on the topic AND the
// matching apns-push-type header; either alone is rejected with
// TopicDisallowed, which is a 403 that reads like a credential problem and is
// not one. Deriving the suffix here rather than adding a second config value
// means the two topics cannot drift apart in an environment file.
func activityTopic(bundleTopic string) string {
	return bundleTopic + liveActivityTopicSuffix
}

// SendActivity delivers one ActivityKit remote update, retrying once on a
// network error or 5xx, and maps APNs rejections onto ErrUnregistered /
// ErrThrottled exactly as Send does.
//
// ErrUnregistered here means the ACTIVITY is gone — dismissed by the rider,
// ended by the app, or expired — not that the phone is gone. The caller drops
// the go_live_activities row, and the device registry is untouched.
func (c *Client) SendActivity(ctx context.Context, n ActivityNotification) error {
	body, err := buildActivityPayload(n)
	if err != nil {
		return err
	}

	return c.deliver(ctx, apnsMessage{
		deviceToken: n.ActivityToken,
		sandbox:     n.Sandbox,
		pushType:    pushTypeLiveActivity,
		topic:       activityTopic(c.topic),
		priority:    n.priority(),
		// Expire the push at the moment its content stops being trustworthy.
		// A queued update that only reaches the phone after its stale-date
		// would overwrite the Activity with a state ActivityKit is about to
		// mark stale anyway — worse than not arriving, because it resets the
		// staleness clock on information that has already expired.
		expiration: strconv.FormatInt(n.StaleDate().Unix(), 10),
		body:       body,
	})
}

// DismissAfter is how long a COMPLETED ride's Activity lingers before iOS
// removes it.
//
// MYR-194: the rider should get to look at the arrival state rather than have
// it vanish the instant the owner taps "Dropped off". Fifteen minutes is long
// enough to check the fare-free summary after walking away and short enough
// that it is gone by the next ride.
const DismissAfter = 15 * time.Minute

// DismissPromptly is the linger for the unhappy terminal states — declined,
// cancelled, and a reservation that expired.
//
// Not zero, deliberately: an Activity dismissed the same instant it is ended
// can disappear before the rider's eyes reach it, and "my ride vanished" is a
// worse experience than the bad news itself. Thirty seconds is one glance.
const DismissPromptly = 30 * time.Second
