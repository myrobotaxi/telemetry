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
		expiration:  strconv.FormatInt(activityExpiration(n).Unix(), 10),
		body:        body,
	})
}

// endPushRetention is how long APNs holds an undelivered `end` push for a phone
// that is off or out of signal. A day is far longer than any ride and far
// shorter than the ActivityKit ceiling; it exists to outlast a flat battery or
// an overnight flight, not to be precise.
const endPushRetention = 24 * time.Hour

// activityExpiration is the apns-expiration instant for one update, and the two
// shapes want OPPOSITE things from it.
//
// AN UPDATE EXPIRES AT ITS STALE-DATE. A queued ETA refresh that reaches the
// phone after its content stopped being trustworthy is worse than one that
// never arrives: it overwrites the Activity with a state ActivityKit was about
// to mark stale anyway, resetting the staleness clock on expired information.
// Late is worthless here, so we tell Apple to drop it.
//
// AN `end` MUST OUTLIVE THE PHONE BEING OFFLINE. It is the only push in this
// system with no successor — the rows are tombstoned the moment it is sent, the
// ticker will never look at them again, and nothing retries. Pinned to the
// stale-date it would be discarded by APNs after ~3 minutes, and a rider whose
// phone was in a tunnel when their ride was declined would be left with a lock
// screen reading "your car is on its way" until ActivityKit's own ceiling
// removed it hours later.
//
// It is NOT pinned to the dismissal-date, which is the tempting answer and the
// wrong one. The dismissal-date is 30 SECONDS for the unhappy endings
// (DismissPromptly) — pinning to it would make the most important push in the
// feature the shortest-lived one, worse than the bug being fixed. And an end
// that arrives after its dismissal-date is not wasted: a dismissal-date in the
// past tells iOS to remove the Activity at once, which is exactly the outcome
// wanted for a card that has been lying since the tunnel. So the floor is a
// day, and a dismissal-date is only honoured when it is even later.
//
// Everything is computed off n.Timestamp, not the wall clock, so the header,
// the `aps.timestamp` and the stale-date in the body all describe one instant.
func activityExpiration(n ActivityNotification) time.Time {
	if n.Event != ActivityEventEnd {
		return n.StaleDate()
	}
	retainUntil := n.Timestamp.Add(endPushRetention)
	if n.DismissalDate != nil && n.DismissalDate.After(retainUntil) {
		return *n.DismissalDate
	}
	return retainUntil
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
