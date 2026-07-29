package push

import (
	"context"
	"errors"
	"log/slog"
)

// Fan-out and delivery for the Notifier. Split from notifier.go to keep both
// files inside the 300-line cap.

// fanOut resolves one ride party's devices and sends the alert to each. Every
// failure here is logged and swallowed: a notification is best-effort garnish
// on a ride that has already happened.
func (n *Notifier) fanOut(ctx context.Context, userID, rideID, topic string, a alert) {
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

	devices, err := n.devices.DevicesForUser(ctx, userID)
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

	if err := n.devices.DeleteDeviceToken(delCtx, deviceToken); err != nil {
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
