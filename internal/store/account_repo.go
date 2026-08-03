// Package store — AccountRepo stores Tesla OAuth tokens as ciphertext
// only (NFR-3.23, NFR-3.25, MYR-433).
//
// Read path: `<col>_enc` (AES-256-GCM ciphertext) is the ONLY source.
// Decryption happens in-process via cryptox.Encryptor; the SDK and HTTP
// layer never see ciphertext. There is no plaintext fallback: MYR-433
// retired it, because a fallback is indistinguishable from a leak — the
// plaintext column has to be readable for the fallback to work.
//
// Write path: every Account write encrypts the new token via
// cryptox.Encryptor.EncryptString and updates `<col>_enc` alone.
//
// The pre-MYR-433 dual-write existed so a rolled-back binary could still
// read a row. That tradeoff is no longer acceptable for this column
// family: these are Tesla fleet-control credentials, and a database leak
// of the plaintext columns hands an attacker the ability to drive a
// user's car. Rollback safety is now provided by the ciphertext being
// readable by both this server and the Next.js app (byte-compatible
// AES-256-GCM, shared ENCRYPTION_KEY), not by a plaintext copy.
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

	"github.com/myrobotaxi/telemetry/internal/cryptox"
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
// Returns ErrTeslaTokenNotFound if no Tesla account row exists or if the
// access token ciphertext is absent.
//
// Every token column is read from its `*_enc` ciphertext (decrypt via
// cryptox.Encryptor). A row whose ciphertext was never written reads as
// absent — the legacy plaintext column is NOT consulted. Rows in that
// state are pre-rollout leftovers; run cmd/backfill-account-tokens to
// seal them BEFORE deploying this binary, and
// cmd/purge-plaintext-columns after.
//
// The id_token is intentionally not surfaced in TeslaOAuthToken today
// (the telemetry server doesn't need it for Fleet API calls) — the
// SELECT pulls it so the encryption contract can be validated
// end-to-end by tests, and so a future caller doesn't have to amend the
// query.
func (r *AccountRepo) GetTeslaToken(ctx context.Context, userID string) (TeslaOAuthToken, error) {
	row := r.pool.QueryRow(ctx, queryTeslaToken, userID)

	var (
		accessEnc  *string
		refreshEnc *string
		idEnc      *string
		expiresAt  *int64
	)

	err := row.Scan(
		&accessEnc,
		&refreshEnc,
		&idEnc,
		&expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}
	if err != nil {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): %w", userID, err)
	}

	access, err := r.resolveTokenValue(accessEnc)
	if err != nil {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): access column: %w", userID, err)
	}
	if access == "" {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): %w", userID, ErrTeslaTokenNotFound)
	}

	refresh, err := r.resolveTokenValue(refreshEnc)
	if err != nil {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): refresh column: %w", userID, err)
	}

	// id column is read but unused; ensure decrypt works so a malformed
	// ciphertext surfaces as a real error rather than silent drift.
	if _, err := r.resolveTokenValue(idEnc); err != nil {
		return TeslaOAuthToken{}, fmt.Errorf("AccountRepo.GetTeslaToken(user=%s): id column: %w", userID, err)
	}

	return TeslaOAuthToken{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
	}, nil
}

// UpdateTeslaToken writes a refreshed token set back to the Account table.
// expiresAt is a Unix epoch timestamp. Only the ciphertext columns are
// written (MYR-433). Returns an error if the update affects zero rows
// (user has no Tesla account linked) or if encryption of any token
// fails — an encrypt failure MUST fail the write rather than fall back
// to storing a bare credential.
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
		accessEnc,
		refreshEnc,
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

// resolveTokenValue decrypts one token column. Ciphertext is the only
// accepted source (MYR-433).
//
// Returns ("", nil) when the ciphertext is absent — callers above
// interpret that as "absent" and decide whether to surface
// ErrTeslaTokenNotFound. A present-but-undecryptable ciphertext is a
// real error, never a silent empty: that distinction is what stops a key
// rotation mistake from looking like an unlinked account.
func (r *AccountRepo) resolveTokenValue(enc *string) (string, error) {
	if enc == nil || *enc == "" {
		return "", nil
	}
	v, err := r.encryptor.DecryptString(*enc)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return v, nil
}
