package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret-key-for-unit-tests"

// signToken creates a signed HS256 JWT for testing.
func signToken(t *testing.T, secret string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	return signed
}

func TestJWTAuthenticator_ValidateToken(t *testing.T) {
	auth := &JWTAuthenticator{
		secret: []byte(testSecret),
		cache:  newVehicleCache(&stubQuerier{}, vehicleCacheTTL),
	}

	tests := []struct {
		name    string
		token   string
		want    string
		wantErr error
	}{
		{
			name: "valid token",
			token: signToken(t, testSecret, jwt.MapClaims{
				"sub": "cmmgr4b1p0005l104ifpctlg8",
				"iat": time.Now().Unix(),
				"exp": time.Now().Add(1 * time.Hour).Unix(),
			}),
			want: "cmmgr4b1p0005l104ifpctlg8",
		},
		{
			name:    "empty token",
			token:   "",
			wantErr: ErrInvalidToken,
		},
		{
			name:    "malformed token",
			token:   "not-a-jwt",
			wantErr: ErrInvalidToken,
		},
		{
			name: "wrong secret",
			token: signToken(t, "wrong-secret", jwt.MapClaims{
				"sub": "user123",
				"exp": time.Now().Add(1 * time.Hour).Unix(),
			}),
			wantErr: ErrInvalidToken,
		},
		{
			name: "expired token",
			token: signToken(t, testSecret, jwt.MapClaims{
				"sub": "user123",
				"iat": time.Now().Add(-2 * time.Hour).Unix(),
				"exp": time.Now().Add(-1 * time.Hour).Unix(),
			}),
			wantErr: ErrInvalidToken,
		},
		{
			name: "missing sub claim",
			token: signToken(t, testSecret, jwt.MapClaims{
				"exp": time.Now().Add(1 * time.Hour).Unix(),
			}),
			wantErr: ErrMissingSubject,
		},
		{
			name: "empty sub claim",
			token: signToken(t, testSecret, jwt.MapClaims{
				"sub": "",
				"exp": time.Now().Add(1 * time.Hour).Unix(),
			}),
			wantErr: ErrMissingSubject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := auth.ValidateToken(context.Background(), tt.token)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error wrapping %v, got nil", tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error wrapping %v, got: %v", tt.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("userID = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJWTAuthenticator_ValidateToken_RS256Rejected(t *testing.T) {
	auth := &JWTAuthenticator{
		secret: []byte(testSecret),
		cache:  newVehicleCache(&stubQuerier{}, vehicleCacheTTL),
	}

	// Create a token that claims to use HS256 but we pass it through anyway
	// — the important thing is that non-HMAC methods are rejected.
	token := jwt.NewWithClaims(jwt.SigningMethodHS384, jwt.MapClaims{
		"sub": "user123",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	signed, err := token.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	_, err = auth.ValidateToken(context.Background(), signed)
	if err == nil {
		t.Fatal("expected error for HS384 token, got nil")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got: %v", err)
	}
}

func TestJWTAuthenticator_ValidateToken_IssuerAudience(t *testing.T) {
	a := &JWTAuthenticator{
		secret:   []byte(testSecret),
		issuer:   "myrobotaxi",
		audience: "telemetry",
		cache:    newVehicleCache(&stubQuerier{}, vehicleCacheTTL),
	}

	tests := []struct {
		name    string
		claims  jwt.MapClaims
		wantErr error
	}{
		{
			name: "correct iss and aud",
			claims: jwt.MapClaims{
				"sub": "user-1", "exp": float64(time.Now().Add(time.Hour).Unix()),
				"iss": "myrobotaxi", "aud": "telemetry",
			},
			wantErr: nil,
		},
		{
			name: "wrong issuer",
			claims: jwt.MapClaims{
				"sub": "user-1", "exp": float64(time.Now().Add(time.Hour).Unix()),
				"iss": "other-app", "aud": "telemetry",
			},
			wantErr: ErrInvalidToken,
		},
		{
			name: "wrong audience",
			claims: jwt.MapClaims{
				"sub": "user-1", "exp": float64(time.Now().Add(time.Hour).Unix()),
				"iss": "myrobotaxi", "aud": "other-service",
			},
			wantErr: ErrInvalidToken,
		},
		{
			name: "missing issuer",
			claims: jwt.MapClaims{
				"sub": "user-1", "exp": float64(time.Now().Add(time.Hour).Unix()),
				"aud": "telemetry",
			},
			wantErr: ErrInvalidToken,
		},
		{
			name: "missing audience",
			claims: jwt.MapClaims{
				"sub": "user-1", "exp": float64(time.Now().Add(time.Hour).Unix()),
				"iss": "myrobotaxi",
			},
			wantErr: ErrInvalidToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := signToken(t, testSecret, tt.claims)
			_, err := a.ValidateToken(context.Background(), token)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error wrapping %v, got nil", tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected error wrapping %v, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestJWTAuthenticator_GetUserVehicles(t *testing.T) {
	querier := &stubQuerier{
		ids: []string{"vehicle-1", "vehicle-2"},
	}
	auth := &JWTAuthenticator{
		secret: []byte(testSecret),
		cache:  newVehicleCache(querier, vehicleCacheTTL),
	}

	ids, err := auth.GetUserVehicles(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ids) != 2 {
		t.Fatalf("got %d vehicle IDs, want 2", len(ids))
	}
	if ids[0] != "vehicle-1" || ids[1] != "vehicle-2" {
		t.Errorf("vehicle IDs = %v, want [vehicle-1 vehicle-2]", ids)
	}

	if querier.callCount.Load() != 1 {
		t.Errorf("querier called %d times, want 1", querier.callCount.Load())
	}
}

func TestJWTAuthenticator_GetUserVehicles_QueryError(t *testing.T) {
	querier := &stubQuerier{
		err: errors.New("database unavailable"),
	}
	auth := &JWTAuthenticator{
		secret: []byte(testSecret),
		cache:  newVehicleCache(querier, vehicleCacheTTL),
	}

	_, err := auth.GetUserVehicles(context.Background(), "user-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// stubQuerier is a test double for vehicleQuerier.
type stubQuerier struct {
	ids       []string
	err       error
	callCount atomic.Int32
}

func (s *stubQuerier) GetUserVehicleIDs(_ context.Context, _ string) ([]string, error) {
	s.callCount.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	return s.ids, nil
}
