package main

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// The MYR-373 share-revocation nudge, wired end to end at cmd/ so neither the
// REST handler nor the hub has to know the other exists (the dependency rule
// in CLAUDE.md). Two halves:
//
//	shareAccessBusNotifier  — REST side. The sharing handlers call it the
//	                          moment a revoke or suspend commits; it publishes
//	                          events.ShareAccessRevokedEvent and returns.
//	shareAccessDispatcher   — hub side. Subscribed to that topic; closes the
//	                          grantee's live sessions for the vehicle.
//
// Modelled on the vehicle_deleted pipeline next door, with one deliberate
// difference: this one never touches the receiver, the VIN cache, or the
// user-existence cache. Those exist to react to a car or a person GOING AWAY.
// Here the car is fine, the person is fine, and the owner's own stream must
// keep flowing — the only thing that changed is that one viewer may no longer
// watch. A blunter reuse of RemoveVehicle would have taken the owner down with
// them.

// shareAccessBusNotifier satisfies telemetry.ShareAccessNotifier by publishing
// onto the in-process bus.
type shareAccessBusNotifier struct {
	bus    events.Bus
	logger *slog.Logger
}

func newShareAccessBusNotifier(bus events.Bus, logger *slog.Logger) *shareAccessBusNotifier {
	return &shareAccessBusNotifier{bus: bus, logger: logger}
}

// ShareAccessRevoked publishes the revocation. It is called ON THE REQUEST
// PATH with the owner waiting on their 200, so it must not block: the bus
// fan-out is a non-blocking send onto a buffered per-subscriber channel.
//
// A publish failure is LOGGED, NOT RETURNED, and does not fail the owner's
// request. The mutation itself has already committed and the cache has already
// been busted — the grant really is gone. What a lost publish costs is the
// difference between "the socket dies in milliseconds" and "the socket dies at
// the next revalidation sweep", which is exactly the gap the backstop in
// ws.AccessRevalidator exists to cover. Failing the owner's DELETE because an
// in-process notification did not land would report a revocation that
// succeeded as one that did not.
func (n *shareAccessBusNotifier) ShareAccessRevoked(granteeUserID, vehicleID, reason string) {
	if n.bus == nil || granteeUserID == "" {
		return
	}
	evt := events.NewEvent(events.ShareAccessRevokedEvent{
		GranteeUserID: granteeUserID,
		VehicleID:     vehicleID,
		Reason:        reason,
	})
	if err := n.bus.Publish(context.Background(), evt); err != nil {
		n.logger.Error("share access revocation not published; live socket will lapse at the revalidation backstop",
			slog.String("user_id", granteeUserID),
			slog.String("vehicle_id", vehicleID),
			slog.String("reason", reason),
			slog.Any("error", err),
		)
	}
}

// shareAccessDispatcher closes a grantee's live WebSocket sessions when their
// grant is revoked or suspended.
type shareAccessDispatcher struct {
	hub    *ws.Hub
	logger *slog.Logger
}

func newShareAccessDispatcher(hub *ws.Hub, logger *slog.Logger) *shareAccessDispatcher {
	return &shareAccessDispatcher{hub: hub, logger: logger}
}

// Subscribe registers the dispatcher on events.TopicShareAccessRevoked.
func (d *shareAccessDispatcher) Subscribe(bus events.Bus) (events.Subscription, error) {
	return bus.Subscribe(events.TopicShareAccessRevoked, d.handle)
}

// handle closes the grantee's sessions for the affected vehicle. Everything it
// does is idempotent, so a duplicate event — the same suspension published
// twice, or a sweep arriving on the heels of the nudge — costs nothing.
func (d *shareAccessDispatcher) handle(evt events.Event) {
	payload, ok := evt.Payload.(events.ShareAccessRevokedEvent)
	if !ok {
		d.logger.Warn("share_access_revoked dispatcher: wrong payload type",
			slog.String("topic", string(evt.Topic)),
		)
		return
	}
	if d.hub == nil || payload.GranteeUserID == "" {
		return
	}

	closed := d.hub.RevokeUserAccess(payload.GranteeUserID, payload.VehicleID, payload.Reason)
	d.logger.Info("dispatched share access revocation",
		slog.String("user_id", payload.GranteeUserID),
		slog.String("vehicle_id", payload.VehicleID),
		slog.String("reason", payload.Reason),
		slog.Int("sessions_closed", closed),
	)
}
