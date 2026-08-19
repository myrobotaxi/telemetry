package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/fleetsuspend"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// The owner-inactivity telemetry sweeper (MYR-592, rest-api.md §7.27).
//
// The numbers below are consts here rather than environment settings, matching
// reservation_wiring.go: they are a PRODUCT policy the client set, not an
// operational knob, and putting them in the environment would invite a deploy to
// quietly change how long somebody may be away before their car goes quiet. The
// only environment control is the kill switch.

const (
	// fleetSuspendInterval is how often a pass runs. Hourly against thresholds
	// measured in DAYS, so the cadence decides only how promptly a car crosses
	// its threshold — worst case an owner is warned or suspended up to an hour
	// late, which is invisible against a 24-hour notice period. Anything faster
	// buys nothing and costs a candidate query per replica.
	fleetSuspendInterval = time.Hour

	// fleetSuspendWarnAfter is the owner silence that earns the day-4 push.
	fleetSuspendWarnAfter = 4 * 24 * time.Hour

	// fleetSuspendSuspendAfter is the owner silence that removes the config.
	// Exactly one day after the warning, which is the whole promise the copy
	// makes ("will be disconnected tomorrow").
	fleetSuspendSuspendAfter = 5 * 24 * time.Hour

	// fleetSuspendMaxPerSweep caps one pass. Generous relative to the beta
	// fleet and deliberately finite: the candidate query is ordered
	// oldest-silence-first, so a backlog delays warnings before it delays
	// suspensions, and a pathological fleet cannot turn one tick into an
	// unbounded run of signed Tesla calls.
	fleetSuspendMaxPerSweep = 200

	// fleetSuspendPassTimeout bounds the two pass-level statements.
	fleetSuspendPassTimeout = 30 * time.Second

	// fleetSuspendVehicleTimeout bounds one vehicle's Tesla call plus its
	// writes. Matched to the fleet-config client's own retry budget so a slow
	// upstream is cut off by this rather than by the pass.
	fleetSuspendVehicleTimeout = 30 * time.Second
)

// startFleetSuspendSweeper constructs and starts the sweeper, returning it so a
// caller can drive a pass in a test.
//
// SUSPENSION IS ONLY POSSIBLE WITH A PROXY. Without one the config remover and
// the token source are nil, and the sweeper still WARNS but never suspends —
// stated in its own log line at startup so an operator can see which half is
// live. That is the correct degradation: warning somebody about a disconnect
// that cannot happen is noise, but claiming a suspension we did not perform
// would tell every consumer a streaming, still-billing car is disconnected.
func startFleetSuspendSweeper(ctx context.Context, deps httpRouteDeps, bus events.Bus) *fleetsuspend.Sweeper {
	cfg := deps.cfg
	log := deps.logger.With(slog.String("component", "telemetry-inactivity-sweeper"))

	var configs fleetsuspend.ConfigRemover
	var tokens fleetsuspend.TokenSource
	if cfg.Proxy().URL != "" {
		configs = telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
			BaseURL:    cfg.Proxy().URL,
			HTTPClient: proxyHTTPClient(cfg.Proxy().URL, log),
		}, log.With(slog.String("subcomponent", "fleet")))
		tokens = &fleetSuspendTokenAdapter{
			resolver: newTeslaTokenResolver(cfg, deps.accountRepo, log),
		}
	}

	sweeper := fleetsuspend.NewSweeper(
		&fleetSuspendStoreAdapter{repo: store.NewTelemetrySuspensionRepo(deps.pool)},
		configs,
		tokens,
		bus,
		fleetsuspend.Config{
			Enabled:        cfg.TelemetryInactivitySuspensionEnabled(),
			Interval:       fleetSuspendInterval,
			WarnAfter:      fleetSuspendWarnAfter,
			SuspendAfter:   fleetSuspendSuspendAfter,
			MaxPerSweep:    fleetSuspendMaxPerSweep,
			PassTimeout:    fleetSuspendPassTimeout,
			VehicleTimeout: fleetSuspendVehicleTimeout,
		},
		log,
	)
	go sweeper.Run(ctx)
	return sweeper
}

// fleetSuspendStoreAdapter re-types the store's candidate rows onto the
// sweeper's own Candidate, so internal/fleetsuspend never imports internal/store
// — the same seam reservationStoreAdapter provides for the dispatch sweeper.
type fleetSuspendStoreAdapter struct {
	repo *store.TelemetrySuspensionRepo
}

func (a *fleetSuspendStoreAdapter) ClearReturnedOwnerEpisodes(ctx context.Context, warnBefore time.Time) (int64, error) {
	return a.repo.ClearReturnedOwnerEpisodes(ctx, warnBefore)
}

func (a *fleetSuspendStoreAdapter) ListInactiveOwnerVehicles(
	ctx context.Context, warnBefore time.Time, limit int,
) ([]fleetsuspend.Candidate, error) {
	rows, err := a.repo.ListInactiveOwnerVehicles(ctx, warnBefore, limit)
	if err != nil {
		return nil, err
	}
	out := make([]fleetsuspend.Candidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, fleetsuspend.Candidate{
			VehicleID:       r.VehicleID,
			VIN:             r.VIN,
			OwnerID:         r.OwnerID,
			VehicleName:     r.VehicleName,
			OwnerLastSeenAt: r.OwnerLastSeenAt,
			WarnedAt:        r.WarnedAt,
		})
	}
	return out, nil
}

func (a *fleetSuspendStoreAdapter) MarkWarned(ctx context.Context, vehicleID string, at time.Time) error {
	return a.repo.MarkWarned(ctx, vehicleID, at)
}

func (a *fleetSuspendStoreAdapter) MarkSuspended(ctx context.Context, vehicleID string, at time.Time) error {
	return a.repo.MarkSuspended(ctx, vehicleID, at)
}

// fleetSuspendTokenAdapter narrows the refreshing Tesla token resolver to the
// one string the sweeper needs. The REFRESHING resolver rather than the raw
// stored token, deliberately: an owner four days silent has almost certainly let
// their access token expire, and using the stored one would make every
// suspension fail with a 401 and retry forever.
type fleetSuspendTokenAdapter struct {
	resolver *telemetry.TeslaTokenResolver
}

func (a *fleetSuspendTokenAdapter) AccessToken(ctx context.Context, userID string) (string, error) {
	tok, err := a.resolver.Resolve(ctx, userID)
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}
