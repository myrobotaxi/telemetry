package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// OwnerProvisioner is the single, audited, transactional path that makes a
// go_users-native Apple user into a Prisma-owning Tesla owner (MYR-257,
// docs/architecture/self-serve-onboarding.md). It is deliberately NOT the
// identity module (ADR-001 §4 keeps identity's "User" access read-only) and NOT
// AccountRepo — it is a distinct, narrowly-scoped writer whose only method
// provisions the minimal owner rows behind a COMPLETED Tesla OAuth link.
//
// It writes exactly three Prisma-owned rows in one transaction, all idempotent
// upserts so a re-link never duplicates:
//
//   - "User"     — ON CONFLICT ("id") DO NOTHING. The id is REUSED from the
//     caller's go_users id (the JWT sub), so the existing go_identity_apple
//     binding already resolves to it and no mapping table is needed. An
//     email-matched caller already has a "User" row, so this is a no-op reuse
//     (never a double-provision).
//   - "Settings" — ON CONFLICT ("userId") DO UPDATE, flag teslaLinked=true.
//   - "Account"  — ON CONFLICT ("provider","providerAccountId") DO UPDATE,
//     writing the dual-write-encrypted Tesla tokens (same contract as
//     AccountRepo.UpdateTeslaToken; NFR-3.23/3.25).
//
// No row is written unless the caller supplies live Tesla tokens (proof of
// ownership); the callback calls this only after a successful code→token
// exchange, so a denied/failed link creates no orphan "User".
type OwnerProvisioner struct {
	pool      *pgxpool.Pool
	encryptor cryptox.Encryptor
}

// NewOwnerProvisioner builds the provisioner. The encryptor MUST be non-nil —
// the Account dual-write contract requires every token write to seal ciphertext
// under the active key (mirrors NewAccountRepo).
func NewOwnerProvisioner(pool *pgxpool.Pool, encryptor cryptox.Encryptor) *OwnerProvisioner {
	if encryptor == nil {
		panic("store.NewOwnerProvisioner: encryptor must not be nil")
	}
	return &OwnerProvisioner{pool: pool, encryptor: encryptor}
}

// ProvisionInput carries the resolved caller identity plus the freshly linked
// Tesla token set. UserID is the JWT sub (reused as "User"."id"). ProviderAccountID
// is the Tesla OIDC subject from userinfo. Name/Email are best-effort P1 display
// values (may be empty for an Apple hidden-relay sign-in) — never logged.
type ProvisionInput struct {
	UserID            string
	ProviderAccountID string
	Name              string
	Email             string
	AccessToken       string
	RefreshToken      string
	ExpiresAt         int64
}

// provisionIDRandomBytes sizes the random suffix of a generated cuid: 16 bytes
// -> 32 hex chars + "c" prefix, matching the platform convention (see
// newRideRequestID / identity.newID).
const provisionIDRandomBytes = 16

// newProvisionID generates a cuid-shaped id for a new Settings/Account row.
func newProvisionID() string {
	b := make([]byte, provisionIDRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("c%x", time.Now().UnixNano())
	}
	return "c" + hex.EncodeToString(b)
}

const queryProvisionUser = `
INSERT INTO "User" ("id", "name", "email", "updatedAt")
VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NOW())
ON CONFLICT ("id") DO NOTHING`

const queryProvisionSettings = `
INSERT INTO "Settings" ("id", "userId", "teslaLinked", "updatedAt")
VALUES ($1, $2, TRUE, NOW())
ON CONFLICT ("userId") DO UPDATE
SET "teslaLinked" = TRUE, "updatedAt" = NOW()`

// #nosec G101 -- column-name SQL, not a credential (gosec greps the literal for
// access_token/refresh_token and misflags it as a hardcoded secret).
const queryProvisionAccount = `
INSERT INTO "Account" (
    "id", "userId", "type", "provider", "providerAccountId",
    "access_token", "access_token_enc",
    "refresh_token", "refresh_token_enc",
    "expires_at"
) VALUES ($1, $2, 'oauth', 'tesla', $3, $4, $5, $6, $7, $8)
ON CONFLICT ("provider", "providerAccountId") DO UPDATE
SET "userId"            = EXCLUDED."userId",
    "access_token"      = EXCLUDED."access_token",
    "access_token_enc"  = EXCLUDED."access_token_enc",
    "refresh_token"     = EXCLUDED."refresh_token",
    "refresh_token_enc" = EXCLUDED."refresh_token_enc",
    "expires_at"        = EXCLUDED."expires_at"`

// ProvisionTeslaOwner creates (or idempotently reconciles) the minimal Prisma
// owner rows for the caller and persists the linked Tesla tokens, all in one
// transaction. Safe to call on every successful link — it never duplicates and
// never double-provisions an already-Prisma user.
func (p *OwnerProvisioner) ProvisionTeslaOwner(ctx context.Context, in ProvisionInput) error {
	if strings.TrimSpace(in.UserID) == "" {
		return fmt.Errorf("store.ProvisionTeslaOwner: empty user id")
	}
	if strings.TrimSpace(in.ProviderAccountID) == "" {
		return fmt.Errorf("store.ProvisionTeslaOwner(user=%s): empty providerAccountId", in.UserID)
	}

	accessEnc, err := p.encryptor.EncryptString(in.AccessToken)
	if err != nil {
		return fmt.Errorf("store.ProvisionTeslaOwner(user=%s): encrypt access: %w", in.UserID, err)
	}
	refreshEnc, err := p.encryptor.EncryptString(in.RefreshToken)
	if err != nil {
		return fmt.Errorf("store.ProvisionTeslaOwner(user=%s): encrypt refresh: %w", in.UserID, err)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store.ProvisionTeslaOwner(user=%s): begin: %w", in.UserID, err)
	}
	// Rollback is a no-op after a successful Commit; safe to always defer.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := provisionRows(ctx, tx, in, accessEnc, refreshEnc); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store.ProvisionTeslaOwner(user=%s): commit: %w", in.UserID, err)
	}
	return nil
}

// provisionRows runs the three upserts in a stable table order (User → Settings
// → Account) so concurrent re-links acquire row locks in the same order and
// cannot deadlock.
func provisionRows(ctx context.Context, tx pgx.Tx, in ProvisionInput, accessEnc, refreshEnc string) error {
	if _, err := tx.Exec(ctx, queryProvisionUser, in.UserID, in.Name, in.Email); err != nil {
		return fmt.Errorf("store.ProvisionTeslaOwner(user=%s): upsert User: %w", in.UserID, err)
	}
	if _, err := tx.Exec(ctx, queryProvisionSettings, newProvisionID(), in.UserID); err != nil {
		return fmt.Errorf("store.ProvisionTeslaOwner(user=%s): upsert Settings: %w", in.UserID, err)
	}
	if _, err := tx.Exec(ctx, queryProvisionAccount,
		newProvisionID(), in.UserID, in.ProviderAccountID,
		in.AccessToken, accessEnc,
		in.RefreshToken, refreshEnc,
		in.ExpiresAt,
	); err != nil {
		return fmt.Errorf("store.ProvisionTeslaOwner(user=%s): upsert Account: %w", in.UserID, err)
	}
	return nil
}
