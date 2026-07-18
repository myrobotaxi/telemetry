package identity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeApple is an injectable Apple token validator.
type fakeApple struct {
	claims AppleClaims
	err    error
}

func (f fakeApple) Validate(context.Context, string, string) (AppleClaims, error) {
	return f.claims, f.err
}

// fakeStore is an in-memory Store for service/linkage/handler tests.
type fakeStore struct {
	apple         map[string]AppleIdentity // apple_sub -> binding
	prismaByEmail map[string]string        // lower-email -> Prisma User CUID
	goUsers       map[string]string        // id -> email

	inserted   []AppleIdentity
	touched    []string
	createdIDs []string

	rotateResult RotateResult
	rotateErr    error
	rootInserts  int

	revokeUserID string
	revokeFound  bool

	// profiles/profileErr back GetUserProfile (MYR-243): profiles is keyed by
	// userID -> {name, email}; a missing key behaves like "no row" (empty
	// strings, no error) exactly like PgStore. profileErr, when set, is
	// returned regardless of profiles (simulates a store failure).
	profiles   map[string]userProfile
	profileErr error
}

// userProfile is the fakeStore's minimal stand-in for a profile row.
type userProfile struct {
	name  string
	email string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		apple:         map[string]AppleIdentity{},
		prismaByEmail: map[string]string{},
		goUsers:       map[string]string{},
	}
}

func (s *fakeStore) GetAppleIdentity(_ context.Context, sub string) (AppleIdentity, bool, error) {
	ai, ok := s.apple[sub]
	return ai, ok, nil
}

func (s *fakeStore) InsertAppleIdentity(_ context.Context, ai AppleIdentity) error {
	s.apple[ai.AppleSub] = ai
	s.inserted = append(s.inserted, ai)
	return nil
}

func (s *fakeStore) TouchAppleLogin(_ context.Context, sub, _, _ string) error {
	s.touched = append(s.touched, sub)
	return nil
}

func (s *fakeStore) FindPrismaUserIDByEmail(_ context.Context, email string) (string, bool, error) {
	id, ok := s.prismaByEmail[email]
	return id, ok, nil
}

func (s *fakeStore) CreateGoUser(_ context.Context, id, email, _ string) error {
	s.goUsers[id] = email
	s.createdIDs = append(s.createdIDs, id)
	return nil
}

func (s *fakeStore) InsertRefreshRoot(context.Context, string, string, string, time.Time) error {
	s.rootInserts++
	return nil
}

func (s *fakeStore) RotateRefreshToken(context.Context, string, string, time.Time) (RotateResult, error) {
	return s.rotateResult, s.rotateErr
}

func (s *fakeStore) RevokeFamilyByToken(context.Context, string) (string, bool, error) {
	return s.revokeUserID, s.revokeFound, nil
}

func (s *fakeStore) GetUserProfile(_ context.Context, userID string) (string, string, error) {
	if s.profileErr != nil {
		return "", "", s.profileErr
	}
	p, ok := s.profiles[userID]
	if !ok {
		return "", "", nil
	}
	return p.name, p.email, nil
}

func newTestService(t *testing.T, store Store, apple appleTokenValidator, bootstrap map[string]string) *Service {
	t.Helper()
	ks, err := NewKeystoreFromPEM(testKeyPEM(t))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	return NewService(ServiceConfig{
		Store:             store,
		Apple:             apple,
		Minter:            NewTokenMinter(ks, "myrobotaxi", "telemetry", time.Hour),
		BootstrapEmailMap: bootstrap,
		RefreshTTL:        90 * 24 * time.Hour,
		Logger:            nil,
	})
}

func TestSignIn_ExistingBindingReused(t *testing.T) {
	store := newFakeStore()
	store.apple["apple-sub-1"] = AppleIdentity{AppleSub: "apple-sub-1", UserID: "cexisting", Name: "Old", Email: "old@example.com"}
	svc := newTestService(t, store, fakeApple{claims: AppleClaims{Sub: "apple-sub-1", Email: "new@example.com", EmailVerified: true}}, nil)

	res, err := svc.SignInWithApple(context.Background(), AppleSignInInput{IdentityToken: "x"})
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if res.User.ID != "cexisting" {
		t.Errorf("user id = %q, want cexisting (binding reused, never re-pointed)", res.User.ID)
	}
	if len(store.touched) != 1 {
		t.Errorf("expected last-login touch, got %d", len(store.touched))
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Error("missing tokens")
	}
}

func TestSignIn_BootstrapOverride(t *testing.T) {
	store := newFakeStore()
	// Email-match would MISS (no prisma row), but the bootstrap binds the CUID.
	svc := newTestService(t, store,
		fakeApple{claims: AppleClaims{Sub: "apple-new", Email: "Owner@Example.com", EmailVerified: true}},
		map[string]string{"owner@example.com": "cmmgr4b1p0005l104ifpctlg8"})

	res, err := svc.SignInWithApple(context.Background(), AppleSignInInput{IdentityToken: "x"})
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if res.User.ID != "cmmgr4b1p0005l104ifpctlg8" {
		t.Errorf("user id = %q, want bootstrap CUID", res.User.ID)
	}
	if len(store.inserted) != 1 || store.inserted[0].UserID != "cmmgr4b1p0005l104ifpctlg8" {
		t.Error("apple binding not persisted to bootstrap CUID")
	}
	if len(store.createdIDs) != 0 {
		t.Error("bootstrap override should not mint a go_users row")
	}
}

func TestSignIn_EmailMatch(t *testing.T) {
	store := newFakeStore()
	store.prismaByEmail["owner@example.com"] = "cprismauser"
	svc := newTestService(t, store,
		fakeApple{claims: AppleClaims{Sub: "apple-new", Email: "owner@example.com", EmailVerified: true}}, nil)

	res, err := svc.SignInWithApple(context.Background(), AppleSignInInput{IdentityToken: "x"})
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if res.User.ID != "cprismauser" {
		t.Errorf("user id = %q, want cprismauser (email-matched)", res.User.ID)
	}
	if len(store.createdIDs) != 0 {
		t.Error("email-match should not mint a go_users row")
	}
}

func TestSignIn_UnverifiedEmailDoesNotMatch(t *testing.T) {
	store := newFakeStore()
	store.prismaByEmail["owner@example.com"] = "cprismauser"
	// email present but NOT verified -> must NOT link by email; mints fresh.
	svc := newTestService(t, store,
		fakeApple{claims: AppleClaims{Sub: "apple-new", Email: "owner@example.com", EmailVerified: false}}, nil)

	res, err := svc.SignInWithApple(context.Background(), AppleSignInInput{IdentityToken: "x"})
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if res.User.ID == "cprismauser" {
		t.Error("unverified email must not link to the Prisma user")
	}
	if len(store.createdIDs) != 1 {
		t.Errorf("expected a fresh go_users mint, got %d", len(store.createdIDs))
	}
}

func TestSignIn_FreshMint(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(t, store,
		fakeApple{claims: AppleClaims{Sub: "apple-brand-new", Email: "new@example.com", EmailVerified: true}}, nil)

	res, err := svc.SignInWithApple(context.Background(), AppleSignInInput{IdentityToken: "x", FullName: "Ada Lovelace"})
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if len(store.createdIDs) != 1 {
		t.Fatalf("expected 1 minted user, got %d", len(store.createdIDs))
	}
	if res.User.ID != store.createdIDs[0] {
		t.Errorf("returned id %q != minted id %q", res.User.ID, store.createdIDs[0])
	}
	if res.User.Name != "Ada Lovelace" {
		t.Errorf("name = %q, want first-sign-in FullName", res.User.Name)
	}
}

func TestSignIn_InvalidAppleToken(t *testing.T) {
	svc := newTestService(t, newFakeStore(), fakeApple{err: ErrInvalidAppleToken}, nil)
	if _, err := svc.SignInWithApple(context.Background(), AppleSignInInput{IdentityToken: "x"}); !errors.Is(err, ErrInvalidAppleToken) {
		t.Fatalf("err = %v, want ErrInvalidAppleToken", err)
	}
}

func TestRefresh_Rotated(t *testing.T) {
	store := newFakeStore()
	store.rotateResult = RotateResult{Outcome: RotateRotated, UserID: "cuser", FamilyID: "fam1"}
	svc := newTestService(t, store, fakeApple{}, nil)

	res, err := svc.Refresh(context.Background(), "some-refresh-token")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Error("rotated refresh missing tokens")
	}
	if res.User.ID != "cuser" {
		t.Errorf("user id = %q", res.User.ID)
	}
}

// TestRefresh_ProfileEnrichment covers MYR-243: a successful refresh
// rotation enriches UserInfo.Name/Email from the store on a best-effort
// basis. Enrichment failure must never fail the refresh itself (fail-open
// for enrichment, never for auth).
func TestRefresh_ProfileEnrichment(t *testing.T) {
	tests := []struct {
		name       string
		profiles   map[string]userProfile
		profileErr error
		wantName   string
		wantEmail  string
	}{
		{
			name:      "profile found -> name and email populated",
			profiles:  map[string]userProfile{"cuser": {name: "Ada Lovelace", email: "ada@example.com"}},
			wantName:  "Ada Lovelace",
			wantEmail: "ada@example.com",
		},
		{
			name:      "no binding row -> id-only projection",
			profiles:  map[string]userProfile{}, // no row for "cuser"
			wantName:  "",
			wantEmail: "",
		},
		{
			name:       "store error -> refresh still succeeds id-only",
			profileErr: errors.New("boom: connection reset"),
			wantName:   "",
			wantEmail:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			store.rotateResult = RotateResult{Outcome: RotateRotated, UserID: "cuser", FamilyID: "fam1"}
			store.profiles = tt.profiles
			store.profileErr = tt.profileErr
			svc := newTestService(t, store, fakeApple{}, nil)

			res, err := svc.Refresh(context.Background(), "some-refresh-token")
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if res.AccessToken == "" || res.RefreshToken == "" {
				t.Error("rotated refresh missing tokens")
			}
			if res.User.ID != "cuser" {
				t.Errorf("user id = %q, want cuser", res.User.ID)
			}
			if res.User.Name != tt.wantName {
				t.Errorf("user name = %q, want %q", res.User.Name, tt.wantName)
			}
			if res.User.Email != tt.wantEmail {
				t.Errorf("user email = %q, want %q", res.User.Email, tt.wantEmail)
			}
		})
	}
}

func TestRefresh_ReuseDetected(t *testing.T) {
	store := newFakeStore()
	store.rotateResult = RotateResult{Outcome: RotateReuse, UserID: "cuser", FamilyID: "fam1"}
	svc := newTestService(t, store, fakeApple{}, nil)

	if _, err := svc.Refresh(context.Background(), "spent-token"); !errors.Is(err, ErrRefreshReuseDetected) {
		t.Fatalf("err = %v, want ErrRefreshReuseDetected", err)
	}
}

func TestRefresh_InvalidAndExpired(t *testing.T) {
	for _, outcome := range []RotateOutcome{RotateInvalid, RotateExpired} {
		store := newFakeStore()
		store.rotateResult = RotateResult{Outcome: outcome}
		svc := newTestService(t, store, fakeApple{}, nil)
		if _, err := svc.Refresh(context.Background(), "tok"); !errors.Is(err, ErrInvalidRefreshToken) {
			t.Errorf("outcome %v: err = %v, want ErrInvalidRefreshToken", outcome, err)
		}
	}
}

func TestRefresh_EmptyToken(t *testing.T) {
	svc := newTestService(t, newFakeStore(), fakeApple{}, nil)
	if _, err := svc.Refresh(context.Background(), ""); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("err = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestRevoke(t *testing.T) {
	store := newFakeStore()
	store.revokeFound = true
	store.revokeUserID = "cuser"
	svc := newTestService(t, store, fakeApple{}, nil)
	if err := svc.Revoke(context.Background(), "tok"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// Unknown token: still a silent success (no oracle).
	store.revokeFound = false
	if err := svc.Revoke(context.Background(), "unknown"); err != nil {
		t.Fatalf("Revoke unknown: %v", err)
	}
	if err := svc.Revoke(context.Background(), ""); err != nil {
		t.Fatalf("Revoke empty: %v", err)
	}
}
