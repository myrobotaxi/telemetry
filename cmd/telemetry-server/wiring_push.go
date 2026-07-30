package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/push"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// Push-notification wiring (MYR-186). Composes the APNs sender, the
// ride-lifecycle bus consumer, and the device-registry endpoints. Lives at
// cmd/ (not inside internal/push) so the notifier depends only on small
// consumer-site interfaces — the same boundary the nav-dispatch wiring keeps.

// setupPushNotifier builds the APNs sender (when the deploy carries the
// credentials) and subscribes the notifier to the three ride-lifecycle topics.
//
// KEYLESS IS A SUPPORTED STATE, not a failure. Without APNS_KEY_P8 /
// APNS_KEY_ID the sender is nil, the notifier still subscribes, and every
// would-be notification is logged as skipped. That is exactly the state the
// service is in between this PR shipping and the Fly secrets being set, so it
// must never block startup — an unreachable phone is not a reason to refuse to
// run a telemetry server.
func setupPushNotifier(
	cfg *config.Config,
	bus events.Bus,
	pushRepo *store.PushDeviceRepo,
	prefsRepo *store.PushPrefsRepo,
	vehicleNames *store.VehicleNameRepo,
	logger *slog.Logger,
) (*push.Notifier, error) {
	pushCfg := cfg.Push()
	log := logger.With(slog.String("component", "push"))

	var sender push.Sender
	if pushCfg.Configured() {
		client, err := push.NewClient(pushCfg.KeyP8PEM, pushCfg.KeyID, pushCfg.TeamID, pushCfg.Topic, log)
		if err != nil {
			// A key that is PRESENT but unusable is a real config error: the
			// operator believes push is on. Fail fast rather than silently
			// degrading to the keyless path.
			return nil, fmt.Errorf("push: build apns client: %w", err)
		}
		sender = client
	} else {
		log.Warn("push notifications not configured; sends will be skipped",
			slog.Bool("has_key", pushCfg.KeyP8PEM != ""),
			slog.Bool("has_key_id", pushCfg.KeyID != ""),
		)
	}

	notifier := push.NewNotifier(
		sender,
		&pushDeviceStoreAdapter{repo: pushRepo},
		&pushPrefsAdapter{repo: prefsRepo},
		vehicleNames,
		push.Config{Enabled: pushCfg.Enabled},
		log,
	)
	if err := notifier.Subscribe(bus); err != nil {
		return nil, fmt.Errorf("subscribe push notifier: %w", err)
	}
	return notifier, nil
}

// setupPushDeviceEndpoints mounts the device-registry surface (rest-api.md
// §7.17). Always mounted: a client must be able to register its token before
// the APNs credentials exist, so that the first deploy carrying them can reach
// phones immediately instead of waiting for every app to relaunch.
func setupPushDeviceEndpoints(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "push-devices"))

	handler := push.NewDevicesHandler(deps.authenticator, deps.pushRepo, logger)

	deps.srv.HandleFunc("PUT /api/push/devices", handler.ServeRegister)
	deps.srv.HandleFunc("DELETE /api/push/devices", handler.ServeUnregister)
	logger.Info("push device endpoints enabled (PUT|DELETE /api/push/devices)")
}

// setupPushPrefsEndpoints mounts the notification-preference surface
// (rest-api.md §7.19, MYR-349). Always mounted, for the same reason the device
// registry is: a person must be able to switch a category off whether or not
// this deploy carries the APNs credentials, and the answer has to be stored
// before the notifier that honours it is ever wired.
func setupPushPrefsEndpoints(deps httpRouteDeps) {
	logger := deps.logger.With(slog.String("component", "push-prefs"))

	handler := push.NewPrefsHandler(deps.authenticator, &pushPrefsAdapter{repo: deps.pushPrefsRepo}, logger)

	deps.srv.HandleFunc("GET /api/users/me/push-prefs", handler.ServeGet)
	deps.srv.HandleFunc("PUT /api/users/me/push-prefs", handler.ServePut)
	logger.Info("push preference endpoints enabled (GET|PUT /api/users/me/push-prefs)")
}

// pushPrefsAdapter adapts the store repo onto the two consumer-site interfaces
// internal/push declares — push.PrefStore (the notifier's gate) and
// push.PrefsRegistry (the endpoints) — so internal/push never imports
// internal/store. One adapter serves both because the two interfaces are the
// read half and the read+write half of the same row.
type pushPrefsAdapter struct {
	repo *store.PushPrefsRepo
}

func (a *pushPrefsAdapter) PrefsForUser(ctx context.Context, userID string) (push.Prefs, error) {
	row, err := a.repo.PrefsForUser(ctx, userID)
	if err != nil {
		return push.Prefs{}, fmt.Errorf("push: read prefs: %w", err)
	}
	return pushPrefsFromRow(row), nil
}

func (a *pushPrefsAdapter) UpdatePrefs(ctx context.Context, userID string, update push.PrefsUpdate) (push.Prefs, error) {
	row, err := a.repo.UpdatePrefs(ctx, userID, store.PushPrefsUpdate{
		RideLifecycle:    update.RideLifecycle,
		DriveStarted:     update.DriveStarted,
		DriveCompleted:   update.DriveCompleted,
		ChargingComplete: update.ChargingComplete,
		ViewerJoined:     update.ViewerJoined,
	})
	if err != nil {
		return push.Prefs{}, fmt.Errorf("push: write prefs: %w", err)
	}
	return pushPrefsFromRow(row), nil
}

// pushPrefsFromRow converts the store row to the notifier's own shape. Written
// out field by field rather than by a shared struct so that adding a sixth
// category fails to compile here — the one place where a new column MUST be
// taught to the gate — instead of silently defaulting to false.
func pushPrefsFromRow(row store.PushPrefs) push.Prefs {
	return push.Prefs{
		RideLifecycle:    row.RideLifecycle,
		DriveStarted:     row.DriveStarted,
		DriveCompleted:   row.DriveCompleted,
		ChargingComplete: row.ChargingComplete,
		ViewerJoined:     row.ViewerJoined,
	}
}

// pushDeviceStoreAdapter adapts the store repo to push.DeviceStore, converting
// the store row into the notifier's own Device shape so internal/push never
// imports internal/store.
type pushDeviceStoreAdapter struct {
	repo *store.PushDeviceRepo
}

func (a *pushDeviceStoreAdapter) DevicesForUser(ctx context.Context, userID string) ([]push.Device, error) {
	rows, err := a.repo.DevicesForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("push: list devices: %w", err)
	}
	out := make([]push.Device, 0, len(rows))
	for _, row := range rows {
		out = append(out, push.Device{Token: row.DeviceToken, Sandbox: row.Sandbox})
	}
	return out, nil
}

func (a *pushDeviceStoreAdapter) DeleteDeviceToken(ctx context.Context, deviceToken string) error {
	if err := a.repo.DeleteDeviceToken(ctx, deviceToken); err != nil {
		return fmt.Errorf("push: delete device: %w", err)
	}
	return nil
}
