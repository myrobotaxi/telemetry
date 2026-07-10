package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AppleIdentity is a persisted apple_sub -> user_id binding row.
type AppleIdentity struct {
	AppleSub string
	UserID   string
	Email    string // P1
	Name     string // P1
}

// RotateOutcome classifies the result of a refresh-token rotation attempt.
type RotateOutcome int

const (
	// RotateInvalid: the presented token hash is unknown.
	RotateInvalid RotateOutcome = iota
	// RotateExpired: the token is known and unspent but naturally expired
	// (benign — no family revoke).
	RotateExpired
	// RotateReuse: a spent or revoked token was presented — theft; the whole
	// family has been revoked.
	RotateReuse
	// RotateRotated: the token was valid and has been rotated to a new one.
	RotateRotated
)

// RotateResult carries the locked old-row identity fields the service needs
// to mint the new access token.
type RotateResult struct {
	Outcome  RotateOutcome
	UserID   string
	FamilyID string
}

// PgStore is the identity module's pgx-backed persistence. It owns the
// go_identity_apple / go_users / go_refresh_tokens tables and holds READ-ONLY
// access to the Prisma "User" table for first-sign-in email linkage (CG-DL-9:
// never writes a Prisma table).
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore constructs the store.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

const queryGetAppleIdentity = `
SELECT apple_sub, user_id, COALESCE(email, ''), COALESCE(name, '')
FROM go_identity_apple WHERE apple_sub = $1`

// GetAppleIdentity returns the binding for an Apple subject, if present.
func (s *PgStore) GetAppleIdentity(ctx context.Context, appleSub string) (AppleIdentity, bool, error) {
	var ai AppleIdentity
	err := s.pool.QueryRow(ctx, queryGetAppleIdentity, appleSub).
		Scan(&ai.AppleSub, &ai.UserID, &ai.Email, &ai.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppleIdentity{}, false, nil
	}
	if err != nil {
		return AppleIdentity{}, false, fmt.Errorf("identity.GetAppleIdentity: %w", err)
	}
	return ai, true, nil
}

const queryInsertAppleIdentity = `
INSERT INTO go_identity_apple (apple_sub, user_id, email, name)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''))`

// InsertAppleIdentity persists a first-sign-in binding. The caller has
// already resolved user_id via the LINKAGE precedence.
func (s *PgStore) InsertAppleIdentity(ctx context.Context, ai AppleIdentity) error {
	_, err := s.pool.Exec(ctx, queryInsertAppleIdentity, ai.AppleSub, ai.UserID, ai.Email, ai.Name)
	if err != nil {
		return fmt.Errorf("identity.InsertAppleIdentity: %w", err)
	}
	return nil
}

const queryTouchAppleLogin = `
UPDATE go_identity_apple
SET last_login_at = NOW(),
    email = COALESCE(email, NULLIF($2, '')),
    name  = COALESCE(name,  NULLIF($3, ''))
WHERE apple_sub = $1`

// TouchAppleLogin updates last_login_at on a returning sign-in and backfills
// email/name if Apple supplied them and we did not have them yet. The
// user_id binding is deliberately never updated (ADR-001 §4).
func (s *PgStore) TouchAppleLogin(ctx context.Context, appleSub, email, name string) error {
	_, err := s.pool.Exec(ctx, queryTouchAppleLogin, appleSub, email, name)
	if err != nil {
		return fmt.Errorf("identity.TouchAppleLogin: %w", err)
	}
	return nil
}

// queryFindPrismaUserByEmail reads the Prisma-owned "User" table READ-ONLY
// (CG-DL-9 permits reads, forbids writes). Case-insensitive email match.
const queryFindPrismaUserByEmail = `
SELECT "id" FROM "User" WHERE lower("email") = lower($1) LIMIT 1`

// FindPrismaUserIDByEmail resolves an existing web user's CUID by email. Used
// only on first Apple sign-in when Apple asserts the email is verified.
func (s *PgStore) FindPrismaUserIDByEmail(ctx context.Context, email string) (userID string, found bool, err error) {
	if email == "" {
		return "", false, nil
	}
	err = s.pool.QueryRow(ctx, queryFindPrismaUserByEmail, email).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("identity.FindPrismaUserIDByEmail: %w", err)
	}
	return userID, true, nil
}

const queryCreateGoUser = `
INSERT INTO go_users (id, email, name) VALUES ($1, NULLIF($2, ''), NULLIF($3, ''))`

// CreateGoUser mints a brand-new Apple-native user row (no legacy Prisma
// "User" exists for them). id is a caller-generated cuid.
func (s *PgStore) CreateGoUser(ctx context.Context, id, email, name string) error {
	_, err := s.pool.Exec(ctx, queryCreateGoUser, id, email, name)
	if err != nil {
		return fmt.Errorf("identity.CreateGoUser: %w", err)
	}
	return nil
}
