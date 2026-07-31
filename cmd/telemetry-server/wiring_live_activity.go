package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/push"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// Live Activity wiring (MYR-172).
//
// Two moving parts, started together because they share a notifier: the
// lifecycle consumer (a bus subscription) and the ETA ticker (a timer). The
// split mirrors the MYR-320 service-status monitor — Subscribe for the edges,
// a `go Run(ctx)` loop for the cadence — because that is this service's
// established shape for "react to events AND poll".

// setupLiveActivityNotifier builds the Live Activity consumer and subscribes it
// to the ride-status topic.
//
// It reuses the APNs client the alert notifier already built rather than
// minting a second one: the provider JWT is cached per client with a 40-minute
// TTL and Apple throttles re-mints at 20 minutes, so two clients on one key is
// a way to get 403 ExpiredProviderToken under load for no benefit.
func setupLiveActivityNotifier(
	cfg *config.Config,
	bus events.Bus,
	client *push.Client,
	activityRepo *store.LiveActivityRepo,
	prefsRepo *store.PushPrefsRepo,
	logger *slog.Logger,
) (*push.ActivityNotifier, error) {
	pushCfg := cfg.Push()
	log := logger.With(slog.String("component", "live-activity"))

	// Same typed-nil conversion as the alert notifier: the keyless path keys
	// off the INTERFACE being nil, which a typed nil pointer is not.
	var sender push.ActivitySender
	if client != nil {
		sender = client
	}

	notifier := push.NewActivityNotifier(
		sender,
		&liveActivityStoreAdapter{repo: activityRepo},
		&pushPrefsAdapter{repo: prefsRepo},
		push.Config{Enabled: pushCfg.Enabled},
		log,
	)
	if err := notifier.Subscribe(bus); err != nil {
		return nil, fmt.Errorf("subscribe live activity notifier: %w", err)
	}
	return notifier, nil
}

// startLiveActivityTicker launches the ETA refresh loop.
//
// Started with a bare `go Run(ctx)` and no drain on shutdown, matching the
// reservation sweeper: a tick interrupted mid-pass costs one ETA refresh, and
// the next process to come up re-lists the same Activities within a tick.
// The LIVE_ACTIVITY_TICKER_ENABLED switch defaults ON: the periodic refresh IS
// the feature, not an optimisation, and a Live Activity whose ETA never moves
// is the thing MYR-194's staleness policy exists to make survivable rather than
// the intended state.
func startLiveActivityTicker(
	ctx context.Context,
	cfg *config.Config,
	notifier *push.ActivityNotifier,
	activityRepo *store.LiveActivityRepo,
	logger *slog.Logger,
) {
	ticker := push.NewActivityTicker(
		notifier,
		&liveActivityStoreAdapter{repo: activityRepo},
		push.TickerConfig{Enabled: cfg.Push().LiveActivityTicker},
		logger.With(slog.String("component", "live-activity-ticker")),
	)
	go ticker.Run(ctx)
}

// liveActivityStoreAdapter adapts the store repo onto the two interfaces
// internal/push declares — push.ActivityStore (the lifecycle fan-out) and
// push.ActivityLegStore (the ticker) — so internal/push never imports
// internal/store. One adapter serves both because they are two read shapes over
// one table.
type liveActivityStoreAdapter struct {
	repo *store.LiveActivityRepo
}

func (a *liveActivityStoreAdapter) ActivitiesForRide(ctx context.Context, rideRequestID string) ([]push.Activity, error) {
	rows, err := a.repo.ActivitiesForRide(ctx, rideRequestID)
	if err != nil {
		return nil, fmt.Errorf("live activity: list registrations: %w", err)
	}
	out := make([]push.Activity, 0, len(rows))
	for _, row := range rows {
		out = append(out, activityFromRow(row))
	}
	return out, nil
}

func (a *liveActivityStoreAdapter) EndActivitiesForRide(ctx context.Context, rideRequestID string) (int64, error) {
	n, err := a.repo.EndActivitiesForRide(ctx, rideRequestID)
	if err != nil {
		return 0, fmt.Errorf("live activity: end registrations: %w", err)
	}
	return n, nil
}

func (a *liveActivityStoreAdapter) DeleteActivityToken(ctx context.Context, token string) error {
	if err := a.repo.DeleteActivityToken(ctx, token); err != nil {
		return fmt.Errorf("live activity: delete registration: %w", err)
	}
	return nil
}

func (a *liveActivityStoreAdapter) RideContextFor(ctx context.Context, rideRequestID string) (push.RideContext, error) {
	row, err := a.repo.ActivityContextForRide(ctx, rideRequestID)
	if err != nil {
		return push.RideContext{}, fmt.Errorf("live activity: read ride context: %w", err)
	}
	return push.RideContext{
		Status:      string(row.Status),
		VehicleName: row.VehicleName,
		Destination: row.DropoffLabel,
		ETAMinutes:  row.ETAMinutes,
	}, nil
}

func (a *liveActivityStoreAdapter) ActiveLegs(ctx context.Context, limit int) ([]push.ActivityLeg, error) {
	rows, err := a.repo.ListActiveLegActivities(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("live activity: list active legs: %w", err)
	}
	out := make([]push.ActivityLeg, 0, len(rows))
	for _, row := range rows {
		out = append(out, push.ActivityLeg{
			Activity: activityFromRow(row.LiveActivity),
			RideContext: push.RideContext{
				Status:      string(row.Status),
				VehicleName: row.VehicleName,
				Destination: row.DropoffLabel,
				ETAMinutes:  row.ETAMinutes,
			},
		})
	}
	return out, nil
}

func (a *liveActivityStoreAdapter) SweepStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	n, err := a.repo.SweepStaleActivities(ctx, olderThan)
	if err != nil {
		return 0, fmt.Errorf("live activity: sweep registrations: %w", err)
	}
	return n, nil
}

// activityFromRow converts a store row to the notifier's own shape. Written out
// field by field rather than by a shared struct so that a new column MUST be
// taught to the sender here, instead of silently arriving as a zero value.
func activityFromRow(row store.LiveActivity) push.Activity {
	return push.Activity{
		RideRequestID: row.RideRequestID,
		UserID:        row.UserID,
		Token:         row.ActivityPushToken,
		Sandbox:       row.Sandbox,
	}
}
