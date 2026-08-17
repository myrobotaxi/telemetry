package main

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// setupProfileNameEndpoint mounts PATCH /api/users/me — the caller edits their
// OWN display name (MYR-581, rest-api.md §7.23, data-lifecycle.md §1.4).
//
// The sibling verb on the same resource, DELETE /api/users/me, is mounted in
// wiring_account_deletion.go. Like it, this route is ALWAYS mounted: the whole
// operation is one local database UPDATE, so there is no deployment shape in
// which exposing it is unsafe.
func setupProfileNameEndpoint(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "profile-name"))

	handler := telemetry.NewProfileNameHandler(
		deps.authenticator,
		store.NewProfileNameRepo(deps.pool),
		logger,
	)
	deps.srv.HandleFunc("PATCH /api/users/me", handler.ServePatch)
	logger.Info("profile name endpoint enabled (PATCH /api/users/me)")
}
