package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/server"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/teslalink"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// teslaLinkSessionTTL bounds how long a started in-app Tesla link may sit
// before the user completes the browser consent. Short enough to limit the
// window a captured state is useful, long enough for a real consent flow.
const teslaLinkSessionTTL = 10 * time.Minute

// setupTeslaLinkEndpoints mounts the user-facing in-app Tesla OAuth link
// surface (MYR-246): POST /api/tesla/link/start and GET /api/tesla/link/callback.
// It is enabled only when a public redirect base URL AND Tesla OAuth credentials
// are configured; otherwise the endpoints are not mounted (the iOS "Link your
// Tesla" button has no backend until then). Mirrors the fleet-config endpoint's
// "warn + skip when unconfigured" pattern.
func setupTeslaLinkEndpoints(
	cfg *config.Config,
	srv *server.Server,
	authenticator ws.Authenticator,
	accountRepo *store.AccountRepo,
	logger *slog.Logger,
) {
	linkCfg := cfg.TeslaLink()
	if linkCfg.RedirectBaseURL == "" {
		logger.Warn("in-app Tesla link disabled: TESLA_LINK_REDIRECT_BASE_URL not set")
		return
	}
	if cfg.TeslaOAuth().ClientID == "" || cfg.TeslaOAuth().ClientSecret == "" {
		logger.Warn("in-app Tesla link disabled: AUTH_TESLA_ID / AUTH_TESLA_SECRET not set")
		return
	}

	redirectURI := linkCfg.RedirectBaseURL + "/api/tesla/link/callback"
	handler := teslalink.NewHandler(
		authenticator,
		&teslaLinkAccountAdapter{repo: accountRepo},
		teslalink.NewSessionStore(teslaLinkSessionTTL),
		teslalink.Config{
			ClientID:       cfg.TeslaOAuth().ClientID,
			ClientSecret:   cfg.TeslaOAuth().ClientSecret,
			RedirectURI:    redirectURI,
			AppRedirectURL: linkCfg.AppRedirectURL,
		},
		logger.With(slog.String("component", "tesla-link")),
	)

	srv.HandleFunc("POST /api/tesla/link/start", handler.ServeStart)
	srv.HandleFunc("GET /api/tesla/link/callback", handler.ServeCallback)

	logger.Info("in-app Tesla link endpoints enabled",
		slog.String("redirect_uri", redirectURI),
		slog.String("app_redirect", linkCfg.AppRedirectURL),
	)
}

// teslaLinkAccountAdapter adapts store.AccountRepo to teslalink.AccountLinker,
// translating the store's ErrTeslaTokenNotFound (zero rows updated => the user
// has no Tesla Account row) into teslalink.ErrAccountNotProvisioned so the
// callback can classify it as account_not_provisioned. This keeps the
// teslalink package free of an internal/store dependency.
type teslaLinkAccountAdapter struct {
	repo *store.AccountRepo
}

func (a *teslaLinkAccountAdapter) UpdateTeslaToken(ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64) error {
	err := a.repo.UpdateTeslaToken(ctx, userID, accessToken, refreshToken, expiresAt)
	if errors.Is(err, store.ErrTeslaTokenNotFound) {
		return teslalink.ErrAccountNotProvisioned
	}
	return err
}
