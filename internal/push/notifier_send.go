package push

import (
	"context"
	"errors"
	"log/slog"
)

// Fan-out and delivery for the Notifier. Split from notifier.go to keep both
// files inside the 300-line cap.

// delivery is everything about ONE fan-out that is not the copy: who it is for,
// what it is about, and — since MYR-349 — which switch in their Settings turns
// it off. Grouped into a struct because four positional strings at a call site
// is exactly how a topic ends up in the ride-id slot.
type delivery struct {
	// userID is the RECIPIENT — the owner for a new request, the rider for a
	// status change or a due reservation. The preference consulted is theirs,
	// never the other party's.
	userID string
	rideID string
	topic  string
	// category is the preference this notification answers to. Every fan-out
	// site must name one; there is no unset value that means "always send".
	category Category
}

// fanOut resolves one ride party's devices and sends the alert to each. Every
// failure here is logged and swallowed: a notification is best-effort garnish
// on a ride that has already happened.
func (n *Notifier) fanOut(ctx context.Context, d delivery, a alert) {
	userID, rideID, topic := d.userID, d.rideID, d.topic
	if userID == "" {
		return
	}

	if !n.active() {
		// Keyless or kill-switched. Log the intent so an operator can see the
		// pipeline is alive and only the delivery is missing — this is the
		// normal state before APNS_KEY_P8 is set on the deploy.
		n.logger.Info("push skipped",
			slog.String("topic", topic),
			slog.String("ride_id", rideID),
			slog.String("user_id", userID),
			slog.Bool("push_enabled", n.cfg.Enabled),
			slog.Bool("apns_configured", n.sender != nil),
		)
		return
	}

	// MYR-349 — the recipient's own switch, checked BEFORE the device lookup so
	// a silenced category costs one point read instead of a fan-out.
	if !n.allowed(ctx, userID, d.category, topic) {
		return
	}

	devices, err := n.stores.devices.DevicesForUser(ctx, userID)
	if err != nil {
		n.logger.Error("push: device lookup failed",
			slog.String("topic", topic),
			slog.String("ride_id", rideID),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return
	}
	if len(devices) == 0 {
		n.logger.Debug("push: no devices registered",
			slog.String("topic", topic),
			slog.String("user_id", userID),
		)
		return
	}

	var delivered int
	for _, d := range devices {
		if n.send(ctx, d, rideID, topic, a) {
			delivered++
		}
	}

	// P1 discipline: the audit line carries opaque ids and counts only — never
	// a device token, and never the alert copy, which embeds a first name.
	n.logger.Info("push sent",
		slog.String("topic", topic),
		slog.String("ride_id", rideID),
		slog.String("user_id", userID),
		slog.Int("devices", len(devices)),
		slog.Int("delivered", delivered),
	)
}

// allowed reports whether the recipient wants this category of notification
// (MYR-349). It is the ONE gate; every send site reaches Apple through it.
//
// IT FAILS OPEN, in all three of the ways it can fail: no PrefStore wired, a
// lookup error, and an account with no stored row. All three resolve to
// DefaultPrefs — everything on, i.e. the exact pre-MYR-349 behaviour.
//
// That direction is not a shortcut, it is the product decision. This package's
// standing rule is that a duplicate notification is a minor annoyance to a
// human whereas a missed one is a rider standing on a sidewalk, and a
// preference gate that failed CLOSED would convert every transient database
// hiccup into platform-wide silence — with no error surfacing anywhere, because
// nothing about a ride waits on push. The cost of failing open is bounded and
// visible: somebody who switched a category off might occasionally receive it.
func (n *Notifier) allowed(ctx context.Context, userID string, category Category, topic string) bool {
	if n.stores.prefs == nil {
		return true
	}

	prefs, err := n.stores.prefs.PrefsForUser(ctx, userID)
	if err != nil {
		n.logger.Error("push: prefs lookup failed; sending anyway",
			slog.String("topic", topic),
			slog.String("category", string(category)),
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return true
	}
	if prefs.Allows(category) {
		return true
	}

	// P0 throughout: an opaque cuid and a category name. Logged at Info rather
	// than Debug because "the notification you expected never arrived" is a
	// support question, and this line is the whole answer to it.
	n.logger.Info("push suppressed by preference",
		slog.String("topic", topic),
		slog.String("category", string(category)),
		slog.String("user_id", userID),
	)
	return false
}

// send delivers to one device and applies the APNs feedback: a permanently
// rejected token is removed from the registry so the next ride does not retry
// a phone that no longer exists. Reports whether the send succeeded.
func (n *Notifier) send(ctx context.Context, d Device, rideID, topic string, a alert) bool {
	err := n.sender.Send(ctx, Notification{
		DeviceToken: d.Token,
		Sandbox:     d.Sandbox,
		Title:       a.title,
		Body:        a.body,
		RideID:      rideID,
	})
	if err == nil {
		return true
	}

	if errors.Is(err, ErrUnregistered) {
		n.dropDevice(ctx, d.Token, topic)
		return false
	}

	n.logger.Warn("push: send failed",
		slog.String("topic", topic),
		slog.String("ride_id", rideID),
		slog.String("device_token_prefix", tokenPrefix(d.Token)),
		slog.String("error", err.Error()),
	)
	return false
}

// dropDevice removes a token APNs reported as permanently dead. The delete
// runs on a context DETACHED from the fan-out's, which may already be at its
// deadline — precisely when the registry most needs the correction to land.
func (n *Notifier) dropDevice(ctx context.Context, deviceToken, topic string) {
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()

	if err := n.stores.devices.DeleteDeviceToken(delCtx, deviceToken); err != nil {
		n.logger.Error("push: failed to delete unregistered device",
			slog.String("topic", topic),
			slog.String("device_token_prefix", tokenPrefix(deviceToken)),
			slog.String("error", err.Error()),
		)
		return
	}
	n.logger.Info("push: deleted unregistered device",
		slog.String("topic", topic),
		slog.String("device_token_prefix", tokenPrefix(deviceToken)),
	)
}

// vehicleName resolves a vehicle nickname for the copy, best-effort. A failure
// is logged at debug and yields "", which the copy renders as a generic label
// — a notification with a slightly blander title beats no notification.
func (n *Notifier) vehicleName(ctx context.Context, vehicleID string) string {
	if n.vehicles == nil || vehicleID == "" {
		return ""
	}
	name, err := n.vehicles.VehicleName(ctx, vehicleID)
	if err != nil {
		n.logger.Debug("push: vehicle name lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		return ""
	}
	return name
}
