package identity

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Store is the persistence surface the Service depends on (consumer-site
// interface; PgStore is the production implementation, and tests supply a
// fake). It spans the three Go-owned tables plus the read-only Prisma "User"
// email lookup used for first-sign-in linkage.
type Store interface {
	GetAppleIdentity(ctx context.Context, appleSub string) (AppleIdentity, bool, error)
	InsertAppleIdentity(ctx context.Context, ai AppleIdentity) error
	TouchAppleLogin(ctx context.Context, appleSub, email, name string) error
	FindPrismaUserIDByEmail(ctx context.Context, email string) (string, bool, error)
	CreateGoUser(ctx context.Context, id, email, name string) error

	InsertRefreshRoot(ctx context.Context, tokenHash, familyID, userID string, expiresAt time.Time) error
	RotateRefreshToken(ctx context.Context, oldHash, newHash string, newExpiresAt time.Time) (RotateResult, error)
	RevokeFamilyByToken(ctx context.Context, tokenHash string) (userID string, found bool, err error)
}

// appleTokenValidator is the consumer-site view of AppleValidator.
type appleTokenValidator interface {
	Validate(ctx context.Context, idToken, expectedNonce string) (AppleClaims, error)
}

// Service is the identity module's application core: it validates Apple
// tokens, resolves/binds the user, and mints the access + refresh pair.
type Service struct {
	store      Store
	apple      appleTokenValidator
	minter     *TokenMinter
	audit      *auditLogger
	bootstrap  map[string]string // lower-email -> existing user CUID
	refreshTTL time.Duration
	now        func() time.Time
}

// ServiceConfig bundles the Service's construction inputs. Apple is the
// consumer-site interface so tests can inject a fake validator; the composition
// root passes a *AppleValidator (which satisfies it).
type ServiceConfig struct {
	Store             Store
	Apple             appleTokenValidator
	Minter            *TokenMinter
	BootstrapEmailMap map[string]string
	RefreshTTL        time.Duration
	Logger            *slog.Logger
}

// NewService constructs the identity service. The auth audit logger is built
// internally from Logger so callers never touch the module's audit surface.
func NewService(cfg ServiceConfig) *Service {
	return &Service{
		store:      cfg.Store,
		apple:      cfg.Apple,
		minter:     cfg.Minter,
		audit:      newAuditLogger(cfg.Logger),
		bootstrap:  cfg.BootstrapEmailMap,
		refreshTTL: cfg.RefreshTTL,
		now:        time.Now,
	}
}

// AppleSignInInput is the validated POST /api/auth/apple request.
type AppleSignInInput struct {
	IdentityToken string // Apple identity token (JWT)
	FullName      string // optional; Apple returns the name only on first authorization
	Email         string // optional client-supplied email (advisory only)
	Nonce         string // optional; matched against the token nonce when present
}

// UserInfo is the public user projection returned to the client.
type UserInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// AuthResult is the token pair + user returned by sign-in and refresh.
type AuthResult struct {
	AccessToken  string
	ExpiresIn    int
	RefreshToken string
	User         UserInfo
}

// SignInWithApple validates the Apple identity token, resolves the user (see
// resolveUser), and issues a fresh access + refresh-family pair.
func (s *Service) SignInWithApple(ctx context.Context, in AppleSignInInput) (AuthResult, error) {
	claims, err := s.apple.Validate(ctx, in.IdentityToken, in.Nonce)
	if err != nil {
		return AuthResult{}, fmt.Errorf("identity.SignInWithApple: %w", err) // chain preserves ErrInvalidAppleToken
	}

	userID, name, email, newUser, err := s.resolveUser(ctx, claims, in)
	if err != nil {
		return AuthResult{}, err
	}

	pair, err := s.issuePair(ctx, userID)
	if err != nil {
		return AuthResult{}, err
	}
	s.audit.signIn(userID, newUser)

	pair.User = UserInfo{ID: userID, Name: name, Email: email}
	return pair, nil
}

// Refresh performs single-use rotation with family reuse-detection.
func (s *Service) Refresh(ctx context.Context, rawRefresh string) (AuthResult, error) {
	if rawRefresh == "" {
		return AuthResult{}, ErrInvalidRefreshToken
	}
	oldHash := hashRefreshToken(rawRefresh)
	newRaw, newHash, err := newRefreshToken()
	if err != nil {
		return AuthResult{}, fmt.Errorf("identity.Refresh: %w", err)
	}
	newExpiry := s.now().UTC().Add(s.refreshTTL)

	res, err := s.store.RotateRefreshToken(ctx, oldHash, newHash, newExpiry)
	if err != nil {
		return AuthResult{}, fmt.Errorf("identity.Refresh: %w", err)
	}
	switch res.Outcome {
	case RotateReuse:
		s.audit.reuseDetected(res.UserID, res.FamilyID)
		return AuthResult{}, ErrRefreshReuseDetected
	case RotateInvalid, RotateExpired:
		return AuthResult{}, ErrInvalidRefreshToken
	case RotateRotated:
		access, expiresIn, err := s.minter.MintAccessToken(res.UserID)
		if err != nil {
			return AuthResult{}, fmt.Errorf("identity.Refresh: mint: %w", err)
		}
		s.audit.refresh(res.UserID)
		return AuthResult{
			AccessToken:  access,
			ExpiresIn:    expiresIn,
			RefreshToken: newRaw,
			User:         UserInfo{ID: res.UserID},
		}, nil
	default:
		return AuthResult{}, ErrInvalidRefreshToken
	}
}

// Revoke revokes the whole refresh-token family a token belongs to. It is
// intentionally non-committal about whether the token existed (no oracle):
// an unknown token is a silent success.
func (s *Service) Revoke(ctx context.Context, rawRefresh string) error {
	if rawRefresh == "" {
		return nil
	}
	userID, found, err := s.store.RevokeFamilyByToken(ctx, hashRefreshToken(rawRefresh))
	if err != nil {
		return fmt.Errorf("identity.Revoke: %w", err)
	}
	if found {
		s.audit.revoke(userID)
	}
	return nil
}

// issuePair mints an access token and a new refresh-token family for userID.
func (s *Service) issuePair(ctx context.Context, userID string) (AuthResult, error) {
	access, expiresIn, err := s.minter.MintAccessToken(userID)
	if err != nil {
		return AuthResult{}, fmt.Errorf("identity.issuePair: mint access: %w", err)
	}
	rawRefresh, refreshHash, err := newRefreshToken()
	if err != nil {
		return AuthResult{}, fmt.Errorf("identity.issuePair: %w", err)
	}
	familyID := newID()
	expiry := s.now().UTC().Add(s.refreshTTL)
	if err := s.store.InsertRefreshRoot(ctx, refreshHash, familyID, userID, expiry); err != nil {
		return AuthResult{}, fmt.Errorf("identity.issuePair: persist refresh: %w", err)
	}
	return AuthResult{AccessToken: access, ExpiresIn: expiresIn, RefreshToken: rawRefresh}, nil
}
