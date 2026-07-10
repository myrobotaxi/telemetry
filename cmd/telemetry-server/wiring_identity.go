package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/identity"
	"github.com/myrobotaxi/telemetry/internal/server"
)

// buildKeystore constructs the identity module's ES256 keystore (ADR-001 §2).
// Precedence: a static config key (production) wins; otherwise, in --dev, an
// ephemeral key is generated with a loud warning; otherwise the module is
// disabled and nil is returned (the server stays a pure HS256 validator).
func buildKeystore(cfg *config.Config, devMode bool, logger *slog.Logger) (*identity.Keystore, error) {
	if pem := cfg.Identity().ES256PrivateKeyPEM; pem != "" {
		ks, err := identity.NewKeystoreFromPEM(pem)
		if err != nil {
			return nil, err
		}
		logger.Info("identity ES256 signing key loaded", slog.String("kid", ks.SigningKID()))
		return ks, nil
	}
	if devMode {
		ks, err := identity.NewEphemeralKeystore()
		if err != nil {
			return nil, err
		}
		logger.Warn("identity: DEV EPHEMERAL ES256 KEY — tokens do not survive restart; set AUTH_ES256_PRIVATE_KEY for a stable key",
			slog.String("kid", ks.SigningKID()))
		return ks, nil
	}
	logger.Warn("identity module disabled: AUTH_ES256_PRIVATE_KEY not set (Sign in with Apple + ES256 minting unavailable; HS256 validation unaffected)")
	return nil, nil //nolint:nilnil // nil keystore + nil error is the documented "module disabled" signal
}

// setupIdentityEndpoints wires the identity module's /api/auth/* surface when
// a keystore is present. The Apple sign-in endpoint additionally requires
// APPLE_NATIVE_CLIENT_ID; when unset, /api/auth/apple 404s but refresh /
// revoke / JWKS still serve (a deploy can rotate keys before Apple is live).
func setupIdentityEndpoints(cfg *config.Config, srv *server.Server, keystore *identity.Keystore, pool *pgxpool.Pool, logger *slog.Logger) {
	if keystore == nil {
		return
	}
	idCfg := cfg.Identity()
	appleEnabled := idCfg.AppleClientID != ""

	minter := identity.NewTokenMinter(keystore, cfg.Auth().TokenIssuer, cfg.Auth().TokenAudience, idCfg.AccessTokenTTL)
	appleValidator := identity.NewAppleValidator(idCfg.AppleClientID, "", nil)
	svc := identity.NewService(identity.ServiceConfig{
		Store:             identity.NewPgStore(pool),
		Apple:             appleValidator,
		Minter:            minter,
		BootstrapEmailMap: idCfg.BootstrapEmailToCUID,
		RefreshTTL:        idCfg.RefreshTokenTTL,
		Logger:            logger.With(slog.String("component", "identity")),
	})
	handler := identity.NewHandler(identity.HandlerConfig{
		Service:            svc,
		Keystore:           keystore,
		AppleEnabled:       appleEnabled,
		RateLimitPerMinute: idCfg.AuthRateLimitPerMinute,
		RateLimitBurst:     idCfg.AuthRateLimitBurst,
		Logger:             logger.With(slog.String("component", "auth-endpoints")),
	})

	srv.HandleFunc("POST /api/auth/apple", handler.ServeApple)
	srv.HandleFunc("POST /api/auth/refresh", handler.ServeRefresh)
	srv.HandleFunc("POST /api/auth/revoke", handler.ServeRevoke)
	srv.HandleFunc("GET /api/auth/.well-known/jwks.json", handler.ServeJWKS)

	logger.Info("identity endpoints enabled",
		slog.Bool("apple_sign_in", appleEnabled),
		slog.String("jwks_kid", keystore.SigningKID()),
	)
}
