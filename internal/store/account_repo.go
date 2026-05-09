// Package store — AccountRepo lives behind a dual-write contract during
// the MYR-62 cross-repo encryption rollout (NFR-3.23, NFR-3.25).
//
// Read path: prefer `<col>_enc` (AES-256-GCM ciphertext) when non-NULL
// and fall back to the legacy plaintext column when the *_enc value has
// not yet been written. Decryption happens in-process via cryptox.Encryptor;
// the SDK and HTTP layer never see ciphertext.
//
// Write path: every Account write encrypts the new token via
// cryptox.Encryptor.EncryptString and updates BOTH `<col>` (plaintext)
// AND `<col>_enc` (ciphertext) in a single SQL statement. The plaintext
// column survives the dual-write window so a deploy of an older binary
// or a roll-back can still read pre-encryption rows. The plaintext
// columns will be dropped in a separate post-rollout migration once
// `account_token_plaintext_remaining_total` reaches zero for all three
// token columns and the rollout is locked in.
//
// The Encryptor MUST be injected via constructor — never call
// cryptox.MustLoad() from within this package. The composition root
// owns the loaded KeySet for the entire process.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tnando/my-robo-taxi-telemetry/internal/cryptox"
)

// AccountRepo reads and updates the Prisma-owned "Account" table for
// OAuth tokens stored during Tesla account linking. Reads token data
// for Fleet API calls; writes updated tokens after auto-refresh.
//
// During the MYR-62 dual-write rollout window the repo encrypts on write
// and prefers ciphertext on read — see the package comment above.
type AccountRepo struct {
	pool      *pgxpool.Pool
	encryptor cryptox.Encryptor
}

// NewAccountRepo creates an AccountRepo backed by the given connection
// pool. The encryptor MUST be non-nil; the dual-write contract requires
// every Account-write to seal the new token under the active write key.
func NewAccountRepo(pool *pgxpool.Pool, encryptor cryptox.Encryptor) *AccountRepo {
	if encryptor == nil {
		// Defensive: a nil Encryptor would silently produce empty *_enc
		// columns, which the read path would then fall back to plaintext
		// for, masking the rollout regression. Fail loudly so the
		// composition root catches this at startup.
		panic("store.NewAccountRepo: encryptor must not be nil")
	}
	return &AccountRepo{pool: pool, encryptor: encryptor}
}

// GetTeslaToken retrieves the Tesla OAuth2 token for the given user.
// Returns ErrTeslaTokenNotFound if no Tesla account row exists or if
// the access token is missing in BOTH plaintext and ciphertext columns.
//
// For each token column the read path prefers ciphertext (decrypt via
// cryptox.Encryptor) and falls back to the legacy plaintext column when
// the *_enc field is NULL. The id_token is intentionally not surfaced
// in TeslaOAuthToken today (the telemetry server doesn't need it for
// Fleet API calls) — the SELECT pulls it so the encryption contract can
// be validated end-to-end by tests, and so a future caller doesn't have
// to amend the query.
func (r *AccountRepo) GetTeslaToken(ctx context.Context, userID string) (TeslaOAuthToken, error) {
	row := r.pool.QueryRow(ctx, queryTeslaToken, userID)

	var (
		accessPT, accessEnc   *string
		refreshPT, refreshEnc *string
		idPT, idEnc           *string
		expiresAt             *int64
	)

	err := row.Scan(
		&accessPT, &accessEnc,
		&refreshPT, &refreshEnc,
		&idPT, &idEnc,
		&expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}
	if err != nil {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): %w", userID, err)
	}

	access, err := r.resolveTokenValue(accessEnc, accessPT)
	if err != nil {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): access column: %w", userID, err)
	}
	if access == "" {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}

	refresh, err := r.resolveTokenValue(refreshEnc, refreshPT)
	if err != nil {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): refresh column: %w", userID, err)
	}

	// id column is read but unused; ensure decrypt works so a malformed
	// ciphertext surfaces as a real error rather than silent drift.
	if _, err := r.resolveTokenValue(idEnc, idPT); err != nil {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): id column: %w", userID, err)
	}

	return TeslaOAuthToken{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
	}, nil
}

// UpdateTeslaToken writes a refreshed token set back to the Account table.
// expiresAt is a Unix epoch timestamp. Both plaintext and ciphertext
// columns are written in the same statement (dual-write). Returns an
// error if the update affects zero rows (user has no Tesla account
// linked) or if encryption of any token fails.
//
// id_token is intentionally not part of the refresh path — Tesla's OAuth
// refresh response does not return it. A separate code path that mints
// id_tokens (e.g., NextAuth signup) would need its own write helper.
func (r *AccountRepo) UpdateTeslaToken(ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64) error {
	accessEnc, err := r.encryptor.EncryptString(accessToken)
	if err != nil {
		return fmt.Errorf("AccountRepo.UpdateTeslaToken(user=%s): encrypt access column: %w", userID, err)
	}
	refreshEnc, err := r.encryptor.EncryptString(refreshToken)
	if err != nil {
		return fmt.Errorf("AccountRepo.UpdateTeslaToken(user=%s): encrypt refresh column: %w", userID, err)
	}

	tag, err := r.pool.Exec(ctx, queryUpdateTeslaToken,
		accessToken, accessEnc,
		refreshToken, refreshEnc,
		expiresAt, userID,
	)
	if err != nil {
		return fmt.Errorf("AccountRepo.UpdateTeslaToken(user=%s): %w", userID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("AccountRepo.UpdateTeslaToken(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}
	return nil
}

// resolveTokenValue implements the read-path preference rule: ciphertext
// wins when present; plaintext is the legacy fallback.
//
// Returns ("", nil) when both inputs are nil — callers above interpret
// that as "absent" and decide whether to surface ErrTeslaTokenNotFound.
// This keeps each token's read logic uniform across access/refresh/id.
func (r *AccountRepo) resolveTokenValue(enc, plaintext *string) (string, error) {
	if enc != nil && *enc != "" {
		v, err := r.encryptor.DecryptString(*enc)
		if err != nil {
			return "", fmt.Errorf("decrypt: %w", err)
		}
		return v, nil
	}
	if plaintext != nil {
		return *plaintext, nil
	}
	return "", nil
}
