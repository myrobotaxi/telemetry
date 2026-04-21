package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

// errRefreshSkipped is returned by refreshToken when the Tesla OAuth
// credentials are not configured or the stored refresh_token is empty.
// Callers interpret this as "keep using the existing access token".
var errRefreshSkipped = errors.New("tesla token refresh skipped")

func runAuth(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("auth requires a subcommand (token | link)")
	}
	switch args[0] {
	case "token":
		return runAuthToken(ctx, args[1:])
	case "link":
		return runAuthLink(ctx, args[1:])
	default:
		return fmt.Errorf("unknown auth subcommand %q", args[0])
	}
}

// authTokenOutput is the JSON shape printed to stdout.
type authTokenOutput struct {
	UserID       string `json:"userId"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    string `json:"expiresAt,omitempty"`
	Refreshed    bool   `json:"refreshed"`
}

func runAuthToken(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("auth token", flag.ContinueOnError)
	userID := fs.String("user-id", "", "MyRoboTaxi user id (Prisma cuid)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireFlag("user-id", *userID); err != nil {
		return err
	}

	logger := newLogger()
	db, err := openDB(ctx, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	accountRepo := store.NewAccountRepo(db.Pool())

	tok, err := accountRepo.GetTeslaToken(ctx, *userID)
	if err != nil {
		return fmt.Errorf("read tesla token: %w", err)
	}

	expiresAt := tokenExpiry(tok.ExpiresAt)
	refreshed := false
	if shouldRefresh(expiresAt) {
		refreshedTok, err := refreshToken(ctx, logger, accountRepo, *userID, tok.RefreshToken)
		switch {
		case err == nil:
			tok.AccessToken = refreshedTok.AccessToken
			tok.RefreshToken = refreshedTok.RefreshToken
			expiresAt = refreshedTok.ExpiresAt()
			refreshed = true
		case errors.Is(err, errRefreshSkipped):
			// Keep the existing (expired) token; user-facing output flags it.
		default:
			return err
		}
	}

	return writeJSON(os.Stdout, authTokenOutput{
		UserID:       *userID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    formatExpiry(expiresAt),
		Refreshed:    refreshed,
	})
}

// tokenExpiry converts the nullable Unix epoch ExpiresAt into a time.Time.
// Returns the zero value when the column was NULL.
func tokenExpiry(expiresAt *int64) time.Time {
	if expiresAt == nil {
		return time.Time{}
	}
	return time.Unix(*expiresAt, 0)
}

// shouldRefresh returns true when the token is expired or will expire in
// the next minute. Tesla tokens are short-lived and the 1-minute skew
// avoids handing out a token that expires mid-request.
func shouldRefresh(expiresAt time.Time) bool {
	if expiresAt.IsZero() {
		return false
	}
	return time.Until(expiresAt) < time.Minute
}

func formatExpiry(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// refreshToken calls the Tesla OAuth refresh endpoint using AUTH_TESLA_ID
// and AUTH_TESLA_SECRET and persists the new token to the DB. Returns
// errRefreshSkipped when credentials are missing or the stored refresh
// token is empty — the caller keeps the existing access token and the
// user must re-link their Tesla account manually.
func refreshToken(
	ctx context.Context,
	logger *slog.Logger,
	accountRepo *store.AccountRepo,
	userID, refreshToken string,
) (telemetry.TeslaRefreshedToken, error) {
	clientID := os.Getenv("AUTH_TESLA_ID")
	clientSecret := os.Getenv("AUTH_TESLA_SECRET")
	if clientID == "" || clientSecret == "" {
		logger.Warn("AUTH_TESLA_ID / AUTH_TESLA_SECRET not set, skipping refresh")
		return telemetry.TeslaRefreshedToken{}, errRefreshSkipped
	}
	if refreshToken == "" {
		logger.Warn("stored refresh_token is empty, skipping refresh")
		return telemetry.TeslaRefreshedToken{}, errRefreshSkipped
	}

	refresher := telemetry.NewTokenRefresher(telemetry.TeslaOAuthConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
	}, logger.With(slog.String("component", "token-refresh")))

	refreshed, err := refresher.Refresh(ctx, refreshToken)
	if err != nil {
		return telemetry.TeslaRefreshedToken{}, fmt.Errorf("refresh tesla token: %w", err)
	}

	if err := accountRepo.UpdateTeslaToken(ctx, userID,
		refreshed.AccessToken, refreshed.RefreshToken,
		refreshed.ExpiresAt().Unix()); err != nil {
		logger.Warn("failed to persist refreshed token",
			slog.String("error", err.Error()),
		)
	}
	return refreshed, nil
}
