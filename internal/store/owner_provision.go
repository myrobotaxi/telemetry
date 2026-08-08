package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
// AccountRepo — it is a distinct, narrowly-scoped writer gated behind a
// COMPLETED Tesla OAuth link.
//
// It converges on the ONE canonical Prisma user for the linked Tesla account
// before writing anything, so it never creates a duplicate user and never
// reassigns an existing Tesla account / vehicle across users. Resolution
// precedence (all inside one transaction):
//
//	(a) An "Account"(provider='tesla', providerAccountId=<sub>) already exists →
//	    the target IS its "userId". The Tesla account's owner is authoritative:
//	    Account.userId is NEVER rewritten. If the caller's identity differs, the
//	    caller's apple binding is converged onto that owner (go_identity_apple).
//	(b) Else a "User" with the resolved email exists → adopt it (reuse its id,
//	    converge the apple binding). No new "User" INSERT — this is what makes
//	    the Apple Hide-My-Email crossover (existing web email surfacing only via
//	    the Tesla account) resolve instead of colliding on User.email @unique.
//	(c) Else INSERT a fresh "User" with the go_users id — now guaranteed
//	    collision-free on both id and email.
//
// Then it upserts "Settings"(teslaLinked=true) and "Account"(dual-write tokens)
// for the canonical user only. Every outcome is audited (userId + opaque
// outcome; no PII/tokens).
type OwnerProvisioner struct {
	pool      *pgxpool.Pool
	encryptor cryptox.Encryptor
	logger    *slog.Logger
}

// NewOwnerProvisioner builds the provisioner. The encryptor MUST be non-nil —
// the Account dual-write contract requires every token write to seal ciphertext
// under the active key (mirrors NewAccountRepo).
func NewOwnerProvisioner(pool *pgxpool.Pool, encryptor cryptox.Encryptor, logger *slog.Logger) *OwnerProvisioner {
	if encryptor == nil {
		panic("store.NewOwnerProvisioner: encryptor must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &OwnerProvisioner{pool: pool, encryptor: encryptor, logger: logger}
}

// ProvisionInput carries the resolved caller identity plus the freshly linked
// Tesla token set. UserID is the caller's JWT sub (their go_users / Prisma id).
// ProviderAccountID is the Tesla OIDC subject from userinfo. Name is a
// best-effort P1 display value.
//
// Email MUST be our own Apple-verified email (or empty for an Apple
// hidden-relay sign-in) — NEVER a Tesla-provided email. It is the trust anchor
// for the (b) email-adoption merge: adopting an existing "User" by email must
// only fire on an address we verified ourselves. An empty Email disables path
// (b) entirely (the caller then converges via the verified Tesla sub (a) or
// provisions a fresh user (c)). Email is also the value persisted to a fresh
// "User".email in case (c). Never logged.
type ProvisionInput struct {
	UserID            string
	ProviderAccountID string
	Name              string
	Email             string
	AccessToken       string
	RefreshToken      string
	ExpiresAt         int64
}

// ProvisionOutcome is the opaque, log-safe classification of a provisioning
// run (P0 — no PII).
type ProvisionOutcome string

const (
	// OutcomeNewUser: a fresh Prisma "User" was created for the go_users id.
	OutcomeNewUser ProvisionOutcome = "new_user"
	// OutcomeAdoptedByEmail: an existing Prisma "User" matched by email was
	// reused; the caller's apple identity was converged onto it.
	OutcomeAdoptedByEmail ProvisionOutcome = "adopted_by_email"
	// OutcomeAdoptedByAccount: the linked Tesla account already belonged to an
	// existing user; that owner was kept and the caller converged onto it.
	OutcomeAdoptedByAccount ProvisionOutcome = "adopted_by_account"
)

// ProvisionResult reports the canonical user the link resolved to and how. The
// caller MUST use CanonicalUserID (not the input UserID) for any follow-on
// owner-scoped work (e.g. vehicle sync), because a converged link owns rows
// under the canonical user, not the caller's original go_users id.
type ProvisionResult struct {
	CanonicalUserID string
	Outcome         ProvisionOutcome
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

// ProvisionTeslaOwner resolves the canonical Prisma user for the linked Tesla
// account and (idempotently, transactionally) upserts the owner's Settings +
// Account. Safe to call on every successful link — it never duplicates a user,
// never reassigns an existing Tesla account across users, and never leaves a
// stale Settings flag on a non-linked user.
func (p *OwnerProvisioner) ProvisionTeslaOwner(ctx context.Context, in ProvisionInput) (ProvisionResult, error) {
	if strings.TrimSpace(in.UserID) == "" {
		return ProvisionResult{}, fmt.Errorf("store.ProvisionTeslaOwner: empty user id")
	}
	if strings.TrimSpace(in.ProviderAccountID) == "" {
		return ProvisionResult{}, fmt.Errorf("store.ProvisionTeslaOwner(user=%s): empty providerAccountId", in.UserID)
	}

	accessEnc, err := p.encryptor.EncryptString(in.AccessToken)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("store.ProvisionTeslaOwner(user=%s): encrypt access: %w", in.UserID, err)
	}
	refreshEnc, err := p.encryptor.EncryptString(in.RefreshToken)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("store.ProvisionTeslaOwner(user=%s): encrypt refresh: %w", in.UserID, err)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("store.ProvisionTeslaOwner(user=%s): begin: %w", in.UserID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	canonical, outcome, err := p.resolveCanonicalUser(ctx, tx, in)
	if err != nil {
		return ProvisionResult{}, err
	}

	if _, err := tx.Exec(ctx, queryProvisionSettings, newProvisionID(), canonical); err != nil {
		return ProvisionResult{}, fmt.Errorf("store.ProvisionTeslaOwner(user=%s): upsert Settings: %w", canonical, err)
	}
	if _, err := tx.Exec(ctx, queryProvisionAccount,
		newProvisionID(), canonical, in.ProviderAccountID,
		accessEnc,
		refreshEnc,
		in.ExpiresAt,
	); err != nil {
		return ProvisionResult{}, fmt.Errorf("store.ProvisionTeslaOwner(user=%s): upsert Account: %w", canonical, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ProvisionResult{}, fmt.Errorf("store.ProvisionTeslaOwner(user=%s): commit: %w", canonical, err)
	}

	// Audit (P0 only: opaque cuids + opaque outcome; never email/name/tokens).
	p.logger.Info("owner_provisioned",
		slog.String("event", "owner_provisioned"),
		slog.String("user_id", canonical),
		slog.String("caller_id", in.UserID),
		slog.Bool("converged", canonical != in.UserID),
		slog.String("outcome", string(outcome)),
	)
	return ProvisionResult{CanonicalUserID: canonical, Outcome: outcome}, nil
}

// resolveCanonicalUser applies the (a)→(b)→(c) precedence, converging the
// caller's apple identity onto an existing owner where one is found and INSERTing
// a fresh "User" only when neither the Tesla account nor the email is known.
func (p *OwnerProvisioner) resolveCanonicalUser(ctx context.Context, tx pgx.Tx, in ProvisionInput) (string, ProvisionOutcome, error) {
	// (a) Existing Tesla account — its owner is authoritative, never rewritten.
	var acctUserID string
	err := tx.QueryRow(ctx, queryFindTeslaAccountUser, in.ProviderAccountID).Scan(&acctUserID)
	if err == nil {
		if acctUserID != in.UserID {
			if rErr := p.rebindApple(ctx, tx, in.UserID, acctUserID); rErr != nil {
				return "", "", rErr
			}
		}
		return acctUserID, OutcomeAdoptedByAccount, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("store.ProvisionTeslaOwner(user=%s): find tesla account: %w", in.UserID, err)
	}

	// (b) Existing user by our Apple-VERIFIED email — adopt it (no colliding
	// User INSERT). in.Email is guaranteed Apple-verified (never a Tesla email),
	// so an identity merge onto an existing User never trusts an unverified
	// address. Empty email skips this path entirely.
	if in.Email != "" {
		var uid string
		err := tx.QueryRow(ctx, queryFindUserByEmail, in.Email).Scan(&uid)
		if err == nil {
			if uid != in.UserID {
				if rErr := p.rebindApple(ctx, tx, in.UserID, uid); rErr != nil {
					return "", "", rErr
				}
			}
			return uid, OutcomeAdoptedByEmail, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", "", fmt.Errorf("store.ProvisionTeslaOwner(user=%s): find user by email: %w", in.UserID, err)
		}
	}

	// (c) Fresh user — collision-free on id (go_users id) and email (no match above).
	if _, err := tx.Exec(ctx, queryProvisionUser, in.UserID, in.Name, in.Email); err != nil {
		return "", "", fmt.Errorf("store.ProvisionTeslaOwner(user=%s): upsert User: %w", in.UserID, err)
	}
	return in.UserID, OutcomeNewUser, nil
}

// rebindApple converges the caller's apple identity (currently bound to
// fromUserID, a fresh go_users id) onto the canonical toUserID. This is the one
// sanctioned re-point of a go_identity_apple binding: it fires only at Tesla-link
// time when ownership/email proves the two identities are the same human.
//
// The left-behind go_users row is NOT harmless, which is what MYR-452 cost us.
// The caller's JWT keeps naming fromUserID until its refresh family expires, and
// DELETE /api/users/me is keyed on exactly that subject — so a deletion run
// against a severed id would revoke nothing, delete nothing, still write its
// audit row and still answer 204, leaving the binding to sign the person back
// into the account they believed they had erased. Recording the convergence is
// therefore part of the same transaction as the re-point: the two facts must
// never be observable apart.
func (p *OwnerProvisioner) rebindApple(ctx context.Context, tx pgx.Tx, fromUserID, toUserID string) error {
	tag, err := tx.Exec(ctx, queryRebindAppleIdentity, toUserID, fromUserID)
	if err != nil {
		return fmt.Errorf("store.ProvisionTeslaOwner: converge apple identity %s->%s: %w", fromUserID, toUserID, err)
	}

	// NO BINDING MOVED, NO EDGE. This guard is load-bearing, not defensive.
	//
	// A convergence edge is an unconditional grant of DELETE authority over
	// toUserID, so it may only be written on evidence that the two ids are the
	// same human — and the evidence IS the binding moving. When it does not
	// move, fromUserID held no binding to begin with, which happens routinely:
	// a caller whose token still names a subject they already converged away
	// from links a SECOND Tesla, owned by somebody else entirely. Writing the
	// edge anyway would point their next deletion at that stranger's account
	// while leaving their own (and its binding, and the resurrection) intact.
	if tag.RowsAffected() == 0 {
		p.logger.Warn("owner_identity_converge_skipped",
			slog.String("event", "owner_identity_converge_skipped"),
			slog.String("from_user_id", fromUserID),
			slog.String("to_user_id", toUserID),
			slog.String("reason", "caller holds no apple binding to move"),
		)
		return nil
	}

	if _, err := tx.Exec(ctx, queryRecordConvergence, toUserID, fromUserID); err != nil {
		return fmt.Errorf("store.ProvisionTeslaOwner: record convergence %s->%s: %w", fromUserID, toUserID, err)
	}
	// The person's LIVE SESSION follows the binding, in this same transaction
	// (MYR-488). It runs here, after the RowsAffected guard above, so it is
	// gated on the identical evidence the convergence edge is: a binding that
	// actually moved. See owner_session_migration.go.
	if err := p.migrateRefreshFamilies(ctx, tx, fromUserID, toUserID); err != nil {
		return err
	}
	p.logger.Info("owner_identity_converged",
		slog.String("event", "owner_identity_converged"),
		slog.String("from_user_id", fromUserID),
		slog.String("to_user_id", toUserID),
	)
	return nil
}
