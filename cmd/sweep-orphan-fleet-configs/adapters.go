// Adapters between cmd-local wiring and the sweep's consumer-site interfaces.
// Split from main.go for the 300-line file cap, mirroring
// cmd/telemetry-server/adapters.go. Every type here is a boundary translation:
// no decision lives in this file.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/fleetorphan"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// orphanStore adapts *store.FleetConfigOrphanRepo to fleetorphan.Store. Four of
// the five methods match outright and are promoted by the embedding; only the
// tombstone read needs its row type re-shaped at the boundary.
type orphanStore struct {
	*store.FleetConfigOrphanRepo
}

func (a orphanStore) ListTombstonedOrphanVINs(
	ctx context.Context, limit int,
) ([]fleetorphan.TombstonedOrphan, error) {
	rows, err := a.FleetConfigOrphanRepo.ListTombstonedOrphanVINs(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]fleetorphan.TombstonedOrphan, 0, len(rows))
	for _, r := range rows {
		out = append(out, fleetorphan.TombstonedOrphan{
			UserID:         r.UserID,
			TeslaVehicleID: r.TeslaVehicleID,
			VIN:            r.VIN,
			RemovedAt:      r.RemovedAt,
		})
	}
	return out, nil
}

// fleetAdapter adapts *telemetry.FleetAPIClient to the sweep's three Tesla
// interfaces, and — the part that matters — maps a Tesla 404 onto
// fleetorphan.ErrNoConfig so an absent config reads as clean rather than as a
// failure.
type fleetAdapter struct {
	client *telemetry.FleetAPIClient
}

func (a *fleetAdapter) ListVehicles(ctx context.Context, token string) ([]fleetorphan.FleetVehicle, error) {
	vs, err := a.client.ListVehicles(ctx, token)
	if err != nil {
		return nil, err
	}
	out := make([]fleetorphan.FleetVehicle, 0, len(vs))
	for _, v := range vs {
		out = append(out, fleetorphan.FleetVehicle{VIN: v.VIN, Owner: v.IsOwner()})
	}
	return out, nil
}

func (a *fleetAdapter) GetTelemetryConfig(
	ctx context.Context, token, vin string,
) (fleetorphan.ConfigStatus, error) {
	res, err := a.client.GetTelemetryConfig(ctx, token, vin)
	if err != nil {
		return fleetorphan.ConfigStatus{}, mapNotFound(err)
	}
	if res == nil {
		return fleetorphan.ConfigStatus{}, fleetorphan.ErrNoConfig
	}
	st := fleetorphan.ConfigStatus{
		// EITHER signal counts. Tesla returns config:null when nothing is set;
		// a non-nil config with synced=false is a pushed-but-unapplied config,
		// which is still a config on file and still billable. Treating only
		// `synced` as presence would silently skip those.
		Configured: res.Response.Synced || res.Response.Config != nil,
	}
	if res.Response.Config != nil {
		st.Exp = res.Response.Config.Exp
	}
	return st, nil
}

func (a *fleetAdapter) DeleteTelemetryConfig(ctx context.Context, token, vin string) error {
	return mapNotFound(a.client.DeleteTelemetryConfig(ctx, token, vin))
}

// mapNotFound turns a Fleet API 404 into fleetorphan.ErrNoConfig and leaves
// every other error alone.
func mapNotFound(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *telemetry.FleetAPIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %d", fleetorphan.ErrNoConfig, apiErr.StatusCode)
	}
	return err
}

// tokenSource adapts *telemetry.TeslaTokenResolver to fleetorphan.TokenSource.
//
// ONLY ErrTeslaTokenUnavailable becomes ErrNoToken. That sentinel means there
// is no Tesla row on file — the deleted-account case the report exists to
// count. ErrTeslaTokenExpired is left as an ordinary error so it lands as
// `failed`: a refresh that failed may succeed on a later run, and folding it
// into `unreachable_no_token` would overstate the permanently-lost set.
type tokenSource struct {
	resolver *telemetry.TeslaTokenResolver
}

func (a *tokenSource) AccessToken(ctx context.Context, userID string) (string, error) {
	tok, err := a.resolver.Resolve(ctx, userID)
	switch {
	case errors.Is(err, telemetry.ErrTeslaTokenUnavailable):
		// The resolver's sentinels carry ids only, never token material.
		return "", fmt.Errorf("%w: %w", fleetorphan.ErrNoToken, err)
	case err != nil:
		return "", err
	}
	return tok.AccessToken, nil
}

// tokenProvider adapts store.AccountRepo to telemetry.TeslaTokenProvider,
// converting the store-layer epoch to a time.Time (the teslaTokenAdapter
// pattern from cmd/telemetry-server, replicated here because that one is
// package-private to the server binary).
type tokenProvider struct {
	repo *store.AccountRepo
}

func (a *tokenProvider) GetTeslaToken(ctx context.Context, userID string) (telemetry.TeslaToken, error) {
	tok, err := a.repo.GetTeslaToken(ctx, userID)
	if err != nil {
		return telemetry.TeslaToken{}, err
	}
	var expiresAt time.Time
	if tok.ExpiresAt != nil {
		expiresAt = time.Unix(*tok.ExpiresAt, 0)
	}
	return telemetry.TeslaToken{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

// tokenUpdater persists a refreshed token set.
//
// THIS IS THE ONE WRITE A DRY RUN CAN STILL MAKE, and it is deliberate: an
// OAuth refresh invalidates the stored refresh token at Tesla, so NOT
// persisting the new pair would break the owner's next server-side Tesla call.
// It touches no fleet config and changes no vehicle state.
type tokenUpdater struct {
	repo *store.AccountRepo
}

func (a *tokenUpdater) UpdateTeslaToken(
	ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64,
) error {
	return a.repo.UpdateTeslaToken(ctx, userID, accessToken, refreshToken, expiresAt)
}
