package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/identity"
	"github.com/myrobotaxi/telemetry/internal/server"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/teslaauth"
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
// are configured; otherwise the endpoints are not mounted.
//
// MYR-257: on callback success the link now PROVISIONS the caller's minimal
// Prisma owner rows (User/Settings/Account) via store.OwnerProvisioner before
// persisting tokens, so a brand-new go_users-native Apple user becomes a working
// owner with no ops step. The optional post-link hook (vehicle sync + fleet
// config push) runs best-effort and is guarded so it only fires against a real
// linked user at runtime — see newOwnerLink / postLinkHook.
func setupTeslaLinkEndpoints(
	cfg *config.Config,
	srv *server.Server,
	authenticator ws.Authenticator,
	pool *pgxpool.Pool,
	encryptor cryptox.Encryptor,
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

	linkLogger := logger.With(slog.String("component", "tesla-link"))
	provisioner := store.NewOwnerProvisioner(pool, encryptor, linkLogger)
	hook := buildOwnerStreamHook(cfg, provisioner, linkLogger)
	linker := &ownerLink{
		provisioner: provisioner,
		profiles:    identity.NewPgStore(pool),
		fetchUserInfo: func(ctx context.Context, accessToken string) (teslaauth.UserInfo, error) {
			return teslaauth.FetchUserInfo(ctx, linkLogger, accessToken)
		},
		hook:   hook,
		logger: linkLogger,
	}

	redirectURI := linkCfg.RedirectBaseURL + "/api/tesla/link/callback"
	handler := teslalink.NewHandler(
		authenticator,
		linker,
		teslalink.NewSessionStore(teslaLinkSessionTTL),
		teslalink.Config{
			ClientID:       cfg.TeslaOAuth().ClientID,
			ClientSecret:   cfg.TeslaOAuth().ClientSecret,
			RedirectURI:    redirectURI,
			AppRedirectURL: linkCfg.AppRedirectURL,
		},
		linkLogger,
	)

	srv.HandleFunc("POST /api/tesla/link/start", handler.ServeStart)
	srv.HandleFunc("GET /api/tesla/link/callback", handler.ServeCallback)

	logger.Info("in-app Tesla link endpoints enabled (self-serve provisioning)",
		slog.String("redirect_uri", redirectURI),
		slog.String("app_redirect", linkCfg.AppRedirectURL),
		slog.Bool("post_link_hook", hook != nil),
	)
}
