package telemetry

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
)

// Refresh statuses on the wire (rest-api.md §7.15). `fresh` means the live
// stream already held current truth and NO Tesla call was made; `refreshed`
// means we woke the car (if needed) and pulled one vehicle_data read.
const (
	RefreshStatusFresh     = "fresh"
	RefreshStatusRefreshed = "refreshed"
)

// streamRecencyReader exposes the MYR-300 per-VIN stream-recency state.
// Satisfied by *ServiceStatusMonitor.
type streamRecencyReader interface {
	LastStreamAt(vin string) (time.Time, bool)
}

// vehicleDataPublisher performs ONE vehicle_data read and republishes it
// through the MYR-260 backfill mapping. Satisfied by
// *ServiceStatusMonitor.RefreshFromVehicleData.
type vehicleDataPublisher interface {
	RefreshFromVehicleData(ctx context.Context, vin, token string) error
}

// vehicleStateProbe reads Tesla's cheap vehicle object, whose `state` field
// ("online"/"asleep"/"offline") is the wake probe. Satisfied by
// *FleetAPIClient.GetVehicle.
type vehicleStateProbe interface {
	GetVehicle(ctx context.Context, token, vin string) (*FleetVehicleState, error)
}

// vehicleWaker is the consumer-site seam over the bounded wake+retry budget in
// internal/commands. Satisfied by *commands.Executor.EnsureAwake — the SAME
// machinery vehicle commands use, deliberately not a second wake loop.
type vehicleWaker interface {
	EnsureAwake(ctx context.Context, vin, token string, probe commands.AwakeProbe) error
}

// RefreshResult is the outcome of an owner-triggered refresh. LastUpdated is
// the instant the returned data reflects: the last live frame's arrival for a
// `fresh` result, or the completion time of the REST read for a `refreshed`
// one.
type RefreshResult struct {
	Status      string
	LastUpdated time.Time
}

// VehicleRefresher implements the MYR-315 owner-triggered refresh: bring one
// vehicle's state up to date on demand, cheaply when possible.
//
// The whole point is that a car whose stream is live needs NOTHING — the app's
// "refresh" button must not wake a sleeping car merely because the user tapped
// it while the data was already current. So the order is strictly:
//
//  1. Stream fresh within the MYR-300 window? Return `fresh`, zero Tesla calls.
//  2. Otherwise wake under the bounded commands budget (probe-first, so an
//     online-but-quiet car costs one cheap read and no wake at all).
//  3. Then exactly ONE vehicle_data read, published through the MYR-260
//     mapping so the values land on the same broadcast + persist path a
//     streamed frame takes.
//
// Step 3 also refreshes the REST-only spec facts carried by vehicle_config —
// `trim` (MYR-279) and `seatCoolingCapable` (MYR-308) — at no extra cost.
type VehicleRefresher struct {
	streams  streamRecencyReader
	tokens   teslaTokenResolver
	fleet    vehicleStateProbe
	waker    vehicleWaker
	backfill vehicleDataPublisher
	now      func() time.Time
	logger   *slog.Logger
}

// VehicleRefreshOption configures optional VehicleRefresher dependencies.
type VehicleRefreshOption func(*VehicleRefresher)

// withRefreshClock injects a clock for deterministic tests.
func withRefreshClock(now func() time.Time) VehicleRefreshOption {
	return func(r *VehicleRefresher) {
		if now != nil {
			r.now = now
		}
	}
}

// NewVehicleRefresher wires the refresh orchestrator.
func NewVehicleRefresher(
	streams streamRecencyReader,
	tokens teslaTokenResolver,
	fleet vehicleStateProbe,
	waker vehicleWaker,
	backfill vehicleDataPublisher,
	logger *slog.Logger,
	opts ...VehicleRefreshOption,
) *VehicleRefresher {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	r := &VehicleRefresher{
		streams:  streams,
		tokens:   tokens,
		fleet:    fleet,
		waker:    waker,
		backfill: backfill,
		now:      time.Now,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// LastStreamAt passes through the monitor's per-VIN stream recency so the
// handler can answer the fresh case (and skip its cooldown) without reaching
// for the monitor itself.
func (r *VehicleRefresher) LastStreamAt(vin string) (time.Time, bool) {
	return r.streams.LastStreamAt(vin)
}

// Refresh brings one vehicle up to date. See the type doc for the ordering
// rationale. Errors are either a *commands.CommandError (already carrying the
// REST status + typed code, e.g. 503 vehicle_asleep once the wake budget is
// spent) or one of this package's sentinels, which the handler maps.
func (r *VehicleRefresher) Refresh(ctx context.Context, userID, vin string) (RefreshResult, error) {
	if at, fresh := r.streams.LastStreamAt(vin); fresh {
		return RefreshResult{Status: RefreshStatusFresh, LastUpdated: at}, nil
	}

	// The resolver already tags its failures with ErrTeslaTokenUnavailable /
	// ErrTeslaTokenExpired, so propagate rather than re-wrap — the handler
	// branches on those sentinels directly.
	tok, err := r.tokens.Resolve(ctx, userID)
	if err != nil {
		return RefreshResult{}, err
	}

	if err := r.waker.EnsureAwake(ctx, vin, tok.AccessToken, r.awakeProbe(tok.AccessToken, vin)); err != nil {
		return RefreshResult{}, err
	}

	if err := r.backfill.RefreshFromVehicleData(ctx, vin, tok.AccessToken); err != nil {
		return RefreshResult{}, err
	}

	r.logger.Info("vehicle refreshed on owner request",
		slog.String("vin", redactVIN(vin)))
	return RefreshResult{Status: RefreshStatusRefreshed, LastUpdated: r.now()}, nil
}

// awakeProbe builds the wake probe: Tesla's cheap vehicle object reports
// `state`, and only "online" can serve a vehicle_data read. Deliberately the
// same read the ServiceStatusMonitor uses on a connectivity edge, so "awake"
// means the same thing in both places.
func (r *VehicleRefresher) awakeProbe(token, vin string) commands.AwakeProbe {
	return func(ctx context.Context) (bool, error) {
		state, err := r.fleet.GetVehicle(ctx, token, vin)
		if err != nil {
			return false, err
		}
		return strings.EqualFold(state.State, "online"), nil
	}
}
