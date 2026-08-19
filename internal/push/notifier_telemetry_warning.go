package push

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// The day-4 owner-inactivity warning (MYR-592) — the first and so far only
// TRANSACTIONAL push in this service.
//
// ── WHY IT IS NOT PREFERENCE-GATED ──────────────────────────────────────────
//
// The client's ruling, recorded at rest-api.md §7.19.4: this is an operational
// account notice, not a subscribable feed. The platform is about to stop a
// service the owner is paying for, it says so one day before it does, and the
// only other channel — the in-app disconnect notice — reaches somebody who by
// definition has not opened the app in four days. A category switch here would
// have to be labelled "do not warn me before I lose live telemetry", which is a
// toggle nobody wants and which silently defeats the feature for whoever finds
// it. See delivery.transactional in notifier_send.go for the three tests a
// notice has to pass to claim this.
//
// ── WHY IT LIVES IN ITS OWN FILE ────────────────────────────────────────────
//
// notifier.go is the RIDE notifier: every handler there takes a ride payload,
// carries a ride id, and answers to CategoryRideLifecycle. This handler shares
// the fan-out machinery and none of those properties, and keeping it adjacent
// would have invited the next reader to give it a ride id "for consistency" —
// which would put an unrelated notification into a ride's collapse group.

// handleTelemetryWarning tells the OWNER their car will stop streaming tomorrow.
//
// THE OWNER IS THE ONLY RECIPIENT. Riders and viewers of a shared car hear
// nothing: suspension follows the owner's inactivity, only the owner can prevent
// it, and telling a rider would be an alarm about a decision they cannot act on.
// That is the same reasoning handleNavUnapplied uses for its owner-only
// audience, arriving from the other direction.
//
// NO RIDE ID, DELIBERATELY. The delivery leaves rideID empty, so collapseID
// returns "" and the send carries no `apns-collapse-id` — which apns_collapse.go
// documents as the correct answer for a notification about something other than
// a ride. Borrowing the vehicle id for that slot would merge this warning with
// whatever else happened to share it, and it would reach the client as a
// `rideId` in userInfo pointing at a ride that does not exist.
//
// ONCE PER EPISODE is enforced UPSTREAM, by the sweeper's warned_at mark, not
// here. The bus is fire-and-forget and cannot be the memory.
func (n *Notifier) handleTelemetryWarning(evt events.Event) {
	ev, ok := evt.Payload.(events.VehicleTelemetryWarningEvent)
	if !ok {
		n.logUnexpectedPayload(evt)
		return
	}
	if ev.OwnerID == "" {
		// A warning with no recipient is a producer bug, not a broadcast.
		n.logger.Warn("push: telemetry warning has no owner; dropping",
			slog.String("vehicle_id", ev.VehicleID))
		return
	}
	a := telemetryWarningAlert(ev.VehicleName)
	n.async(func(ctx context.Context) {
		n.fanOut(ctx, delivery{
			userID: ev.OwnerID,
			topic:  string(evt.Topic),
			// No category: this notice answers to no switch, and naming one it
			// then ignores would leave a reader unsure which is the truth.
			transactional: true,
			// islandAlerts stays false: an owner holds no Live Activity for
			// this, and there is no ride for one to be about.
		}, a)
	})
}
