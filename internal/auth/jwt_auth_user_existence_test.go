package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestJWTAuthenticator_ValidateToken_DeletedUserRejected verifies the
// FR-10.1 fail-closed existence check (MYR-73): a JWT whose `sub`
// has no User row must be rejected with ErrInvalidToken (mapping to
// auth_failed downstream).
func TestJWTAuthenticator_ValidateToken_DeletedUserRejected(t *testing.T) {
	checker := &fakeChecker{existsBy: map[string]bool{}}
	a := &JWTAuthenticator{
		secret:          []byte(testSecret),
		userExistsCache: newUserExistenceCache(checker, time.Hour),
	}

	token := signToken(t, testSecret, jwt.MapClaims{
		"sub": "ghost-user",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	_, err := a.ValidateToken(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for deleted user, got nil")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected wrap of ErrInvalidToken, got: %v", err)
	}
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected wrap of ErrUserNotFound, got: %v", err)
	}
}

// TestJWTAuthenticator_ValidateToken_LiveUserAllowed asserts the same
// token passes when the User row exists.
func TestJWTAuthenticator_ValidateToken_LiveUserAllowed(t *testing.T) {
	checker := &fakeChecker{existsBy: map[string]bool{"alive-user": true}}
	a := &JWTAuthenticator{
		secret:          []byte(testSecret),
		userExistsCache: newUserExistenceCache(checker, time.Hour),
	}

	token := signToken(t, testSecret, jwt.MapClaims{
		"sub": "alive-user",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	got, err := a.ValidateToken(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "alive-user" {
		t.Errorf("userID = %q, want alive-user", got)
	}
}

// TestJWTAuthenticator_ValidateToken_ExistenceCacheSkippedWhenNil
// verifies that an authenticator constructed without
// userExistsCache (e.g., the legacy struct-literal path used by
// some tests) skips the existence check entirely.
func TestJWTAuthenticator_ValidateToken_ExistenceCacheSkippedWhenNil(t *testing.T) {
	a := &JWTAuthenticator{secret: []byte(testSecret)} // no userExistsCache
	token := signToken(t, testSecret, jwt.MapClaims{
		"sub": "any-user",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	got, err := a.ValidateToken(context.Background(), token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "any-user" {
		t.Errorf("userID = %q, want any-user", got)
	}
}

// TestJWTAuthenticator_InvalidateUser asserts the public InvalidateUser
// hook drops the cached existence entry.
func TestJWTAuthenticator_InvalidateUser(t *testing.T) {
	checker := &fakeChecker{existsBy: map[string]bool{"u": true}}
	a := &JWTAuthenticator{
		secret:          []byte(testSecret),
		userExistsCache: newUserExistenceCache(checker, time.Hour),
	}

	token := signToken(t, testSecret, jwt.MapClaims{
		"sub": "u",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	if _, err := a.ValidateToken(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	a.InvalidateUser("u")
	if _, err := a.ValidateToken(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if got := checker.calls.Load(); got != 2 {
		t.Errorf("checker calls = %d, want 2 (InvalidateUser must force refetch)", got)
	}
}
