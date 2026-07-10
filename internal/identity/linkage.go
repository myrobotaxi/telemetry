package identity

import (
	"context"
	"fmt"
	"strings"
)

// resolveUser maps an Apple sign-in to a user CUID, binding it in
// go_identity_apple on first sign-in. It returns the user id, the best-known
// name/email for the client projection, and whether a brand-new user was
// minted.
//
// Precedence (ADR-001 §4, same-DB topology):
//  1. An existing apple_sub binding wins outright — the binding is reused
//     verbatim so a later email change can never re-point an Apple account.
//  2. First sign-in: config bootstrap override (verified email -> CUID).
//  3. First sign-in: verified-email match against the Prisma "User" table.
//  4. First sign-in: mint a fresh Apple-native user in go_users.
func (s *Service) resolveUser(ctx context.Context, claims AppleClaims, in AppleSignInInput) (userID, name, email string, newUser bool, err error) {
	// The email used for linkage MUST be the verified claim (never the
	// client-supplied advisory field). Empty when Apple asserts no verified
	// email (hidden-email / relay without verification).
	verifiedEmail := ""
	if claims.EmailVerified {
		verifiedEmail = strings.TrimSpace(claims.Email)
	}
	// Name is captured from the client on first authorization (Apple returns
	// it only once, outside the token). Email for the projection prefers the
	// verified claim, then the client-supplied advisory value.
	name = strings.TrimSpace(in.FullName)
	projEmail := verifiedEmail
	if projEmail == "" {
		projEmail = strings.TrimSpace(in.Email)
	}

	// (1) Existing binding.
	if existing, found, gErr := s.store.GetAppleIdentity(ctx, claims.Sub); gErr != nil {
		return "", "", "", false, fmt.Errorf("identity.resolveUser: get binding: %w", gErr)
	} else if found {
		// Backfill name/email if we learned them now and lacked them before.
		if tErr := s.store.TouchAppleLogin(ctx, claims.Sub, verifiedEmail, name); tErr != nil {
			return "", "", "", false, fmt.Errorf("identity.resolveUser: touch: %w", tErr)
		}
		return existing.UserID, firstNonEmpty(name, existing.Name), firstNonEmpty(projEmail, existing.Email), false, nil
	}

	// First sign-in — resolve the CUID.
	resolvedID, isNew, rErr := s.resolveFirstSignIn(ctx, verifiedEmail)
	if rErr != nil {
		return "", "", "", false, rErr
	}

	// Bind apple_sub -> user_id, persisting first-sign-in name/email.
	if iErr := s.store.InsertAppleIdentity(ctx, AppleIdentity{
		AppleSub: claims.Sub,
		UserID:   resolvedID,
		Email:    verifiedEmail,
		Name:     name,
	}); iErr != nil {
		return "", "", "", false, fmt.Errorf("identity.resolveUser: bind: %w", iErr)
	}
	return resolvedID, name, projEmail, isNew, nil
}

// resolveFirstSignIn applies the bootstrap-override then email-match then
// fresh-mint precedence and returns the CUID plus whether it is a new user.
func (s *Service) resolveFirstSignIn(ctx context.Context, verifiedEmail string) (userID string, newUser bool, err error) {
	// (2) Bootstrap override — binds a known user even if email-match misses.
	if verifiedEmail != "" && s.bootstrap != nil {
		if cuid, ok := s.bootstrap[strings.ToLower(verifiedEmail)]; ok {
			return cuid, false, nil
		}
	}

	// (3) Verified-email match against the Prisma "User" table (read-only).
	if verifiedEmail != "" {
		if id, found, fErr := s.store.FindPrismaUserIDByEmail(ctx, verifiedEmail); fErr != nil {
			return "", false, fmt.Errorf("identity.resolveFirstSignIn: email match: %w", fErr)
		} else if found {
			return id, false, nil
		}
	}

	// (4) Fresh mint — a genuinely new user with no legacy row.
	id := newID()
	if cErr := s.store.CreateGoUser(ctx, id, verifiedEmail, ""); cErr != nil {
		return "", false, fmt.Errorf("identity.resolveFirstSignIn: mint: %w", cErr)
	}
	return id, true, nil
}

// firstNonEmpty returns the first non-empty of its arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
