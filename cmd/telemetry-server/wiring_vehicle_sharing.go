package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupVehicleSharingEndpoints mounts the MYR-184 vehicle-sharing surface
// (rest-api.md §7.5) — four owner routes and one rider route:
//
//	POST   /api/vehicles/{vehicleId}/invites
//	GET    /api/vehicles/{vehicleId}/invites
//	DELETE /api/invites/{inviteId}
//	POST   /api/invites/{inviteId}/resend
//	POST   /api/invites/redeem
//
// ROUTE ORDERING NOTE: `/api/invites/redeem` and `/api/invites/{inviteId}` do
// not collide — they differ in method (POST vs DELETE) — but
// `/api/invites/{inviteId}/resend` and `/api/invites/redeem` are both POST
// under /api/invites/. Go's ServeMux gives the literal segment precedence over
// the wildcard, and the two patterns have different segment counts anyway, so
// both resolve unambiguously. This is the same precedence the MYR-175
// /api/ride-requests/incoming route relies on.
//
// The authenticator is passed as the cache invalidator: redeeming busts the
// REDEEMER's cached access set so the car appears immediately, and revoking
// busts the REVOKED VIEWER's so access ends immediately rather than at the next
// TTL lapse. See auth.JWTAuthenticator.InvalidateVehicles for the
// single-instance caveat.
func setupVehicleSharingEndpoints(deps httpRouteDeps, vehicles telemetry.VehicleSnapshotReader) {
	logger := deps.logger.With(slog.String("component", "vehicle-sharing"))

	inviteHandler := telemetry.NewShareInviteHandler(
		deps.authenticator,
		vehicles,
		&shareInviteAdapter{repo: deps.shareRepo},
		deps.accessInvalidator,
		logger,
	)
	deps.srv.HandleFunc("POST /api/vehicles/{vehicleId}/invites", inviteHandler.ServeCreate)
	deps.srv.HandleFunc("GET /api/vehicles/{vehicleId}/invites", inviteHandler.ServeList)
	deps.srv.HandleFunc("DELETE /api/invites/{inviteId}", inviteHandler.ServeRevoke)
	deps.srv.HandleFunc("POST /api/invites/{inviteId}/resend", inviteHandler.ServeResend)

	redeemHandler := telemetry.NewShareRedeemHandler(
		deps.authenticator,
		&shareRedeemAdapter{repo: deps.shareRepo},
		&sharedVehicleListerAdapter{repo: deps.vehicleRepo},
		deps.accessInvalidator,
		logger,
	)
	deps.srv.HandleFunc("POST /api/invites/redeem", redeemHandler.ServeHTTP)

	logger.Info("vehicle sharing endpoints enabled (MYR-184)")
}
