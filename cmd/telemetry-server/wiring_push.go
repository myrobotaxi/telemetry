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
// buildAPNsClient constructs the shared APNs client, or returns nil for the
// keyless mode.
//
// ONE client for the whole process, handed to both the alert notifier and the
// Live Activity notifier (MYR-172). Not an economy: the provider JWT is cached
// per client for 40 minutes and Apple throttles re-minting the same key more
// often than every 20, so two clients over one signing key is a way to earn
// 403 ExpiredProviderToken under load for no benefit at all.
func buildAPNsClient(cfg *config.Config, log *slog.Logger) (*push.Client, error) {
	pushCfg := cfg.Push()
	if !pushCfg.Configured() {
		log.Warn("push notifications not configured; sends will be skipped",
			slog.Bool("has_key", pushCfg.KeyP8PEM != ""),
			slog.Bool("has_key_id", pushCfg.KeyID != ""),
		)
		return nil, nil //nolint:nilnil // the keyless mode is a supported state, not an error
	}

	client, err := push.NewClient(pushCfg.KeyP8PEM, pushCfg.KeyID, pushCfg.TeamID, pushCfg.Topic, log)
	if err != nil {
		// A key that is PRESENT but unusable is a real config error: the
		// operator believes push is on. Fail fast rather than silently
		// degrading to the keyless path.
		return nil, fmt.Errorf("push: build apns client: %w", err)
	}
	return client, nil
}

func setupPushNotifier(
	cfg *config.Config,
	bus events.Bus,
	client *push.Client,
	pushRepo *store.PushDeviceRepo,
	prefsRepo *store.PushPrefsRepo,
	activityRepo *store.LiveActivityRepo,
	vehicleNames *store.VehicleNameRepo,
	logger *slog.Logger,
) (*push.Notifier, error) {
	pushCfg := cfg.Push()
	log := logger.With(slog.String("component", "push"))

	// A typed nil *push.Client is NOT a nil push.Sender interface, and the
	// notifier's keyless path keys off the interface being nil. Convert
	// explicitly rather than assigning through.
	var sender push.Sender
	if client != nil {
		sender = client
	}

	// The Live Activity registry, passed WITHOUT an adapter (MYR-413): the repo
	// method's signature already is push.ActivityPresenceStore. Same typed-nil
	// care as the sender above — the gate's fail-open path keys off the
	// INTERFACE being nil, which a typed nil pointer is not.
	var activities push.ActivityPresenceStore
	if activityRepo != nil {
		activities = activityRepo
	}

	notifier := push.NewNotifier(
		sender,
		&pushDeviceStoreAdapter{repo: pushRepo},
		&pushPrefsAdapter{repo: prefsRepo},
		activities,
		vehicleNames,
		push.Config{Enabled: pushCfg.Enabled},
		log,
	).WithRequesterNames(vehicleNames)
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
	// A TYPED NIL is not a nil interface. The notifier's own `prefs == nil`
	// guard cannot see through this adapter: it always receives a non-nil
	// *pushPrefsAdapter, which may hold a nil repo. main.go builds the repo
	// unconditionally today, so this is unreachable — but if it ever stops
	// doing so, the failure without this line is a nil-pointer panic inside a
	// detached fan-out goroutine, which takes the process down over a
	// notification. Fail open, exactly as every other unresolvable preference
	// does.
	if a.repo == nil {
		return push.DefaultPrefs(), nil
	}

	row, err := a.repo.PrefsForUser(ctx, userID)
	if err != nil {
		return push.Prefs{}, fmt.Errorf("push: read prefs: %w", err)
	}
	return pushPrefsFromRow(row), nil
}

func (a *pushPrefsAdapter) UpdatePrefs(ctx context.Context, userID string, update push.PrefsUpdate) (push.Prefs, error) {
	// Same typed-nil guard as PrefsForUser — but the WRITE fails LOUDLY rather
	// than open. A read that cannot resolve a preference falls back to the
	// pre-MYR-349 behaviour and loses nothing; a write that silently discarded
	// somebody's choice and answered 200 would be the original lie restored.
	if a.repo == nil {
		return push.Prefs{}, fmt.Errorf("push: write prefs: no preference store wired")
	}

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
