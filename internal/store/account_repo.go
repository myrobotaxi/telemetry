package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AccountRepo reads and updates the Prisma-owned "Account" table for
// OAuth tokens stored during Tesla account linking. Reads token data
// for Fleet API calls; writes updated tokens after auto-refresh.
type AccountRepo struct {
	pool *pgxpool.Pool
}

// NewAccountRepo creates an AccountRepo backed by the given connection pool.
func NewAccountRepo(pool *pgxpool.Pool) *AccountRepo {
	return &AccountRepo{pool: pool}
}

// GetTeslaToken retrieves the Tesla OAuth2 token for the given user.
// Returns ErrTeslaTokenNotFound if no Tesla account row exists or
// if the stored access_token is NULL.
func (r *AccountRepo) GetTeslaToken(ctx context.Context, userID string) (TeslaOAuthToken, error) {
	row := r.pool.QueryRow(ctx, queryTeslaToken, userID)

	var tok TeslaOAuthToken
	var accessToken, refreshToken *string

	err := row.Scan(&accessToken, &refreshToken, &tok.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}
	if err != nil {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): %w", userID, err)
	}

	if accessToken == nil {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}

	tok.AccessToken = *accessToken
	if refreshToken != nil {
		tok.RefreshToken = *refreshToken
	}
	return tok, nil
}

// UpdateTeslaToken writes a refreshed token set back to the Account table.
// The expiresAt is a Unix epoch timestamp. Returns an error if the update
// affects zero rows (user has no Tesla account linked).
func (r *AccountRepo) UpdateTeslaToken(ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64) error {
	tag, err := r.pool.Exec(ctx, queryUpdateTeslaToken, accessToken, refreshToken, expiresAt, userID)
	if err != nil {
		return fmt.Errorf("AccountRepo.UpdateTeslaToken(user=%s): %w", userID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("AccountRepo.UpdateTeslaToken(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}
	return nil
}
