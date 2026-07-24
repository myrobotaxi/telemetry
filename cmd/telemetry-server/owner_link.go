package main

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/teslaauth"
)

// ownerProvisioner is the consumer-site view of store.OwnerProvisioner.
type ownerProvisioner interface {
	ProvisionTeslaOwner(ctx context.Context, in store.ProvisionInput) (store.ProvisionResult, error)
}

// profileLookup is the best-effort name/email source for the provisioned
// "User" row (identity.PgStore.GetUserProfile). A miss is not an error.
type profileLookup interface {
	GetUserProfile(ctx context.Context, userID string) (name, email string, err error)
}

// userInfoFetcher resolves the Tesla OIDC subject (providerAccountId) from a
// fresh access token. Injected so tests never call auth.tesla.com.
type userInfoFetcher func(ctx context.Context, accessToken string) (teslaauth.UserInfo, error)

// postLinkHook runs after a successful provision to make the owner's car
// stream: server-side vehicle sync + fleet-config push. It is best-effort (a
// failure never fails the link) and nil unless the proxy is configured, so it
// only ever fires against a real linked user at runtime. See ownerStreamHook.
type postLinkHook interface {
	AfterLink(ctx context.Context, userID, accessToken string)
}

// ownerLink implements teslalink.AccountLinker. On a successful in-app Tesla
// link it (1) resolves the Tesla providerAccountId via userinfo, (2)
// transactionally provisions the caller's minimal Prisma owner rows + persists
// the linked tokens (MYR-257), and (3) best-effort triggers the stream hook.
//
// Returning an error here surfaces to the callback as reason=persist_failed;
// because provisioning creates the Account row, account_not_provisioned is no
// longer reachable on the happy path.
type ownerLink struct {
	provisioner   ownerProvisioner
	profiles      profileLookup
	fetchUserInfo userInfoFetcher
	hook          postLinkHook
	logger        *slog.Logger
}

// UpdateTeslaToken satisfies teslalink.AccountLinker. The name is retained from
// the interface (the callback passes the freshly exchanged tokens); the body
// provisions rather than a bare UPDATE.
func (o *ownerLink) UpdateTeslaToken(ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64) error {
	info, err := o.fetchUserInfo(ctx, accessToken)
	if err != nil {
		return err
	}

	// Best-effort P1 display values for the "User" row. Prefer the identity
	// profile (Apple-side), fall back to the Tesla account email. Never logged.
	name, email, perr := o.profiles.GetUserProfile(ctx, userID)
	if perr != nil {
		o.logger.Warn("owner provision: profile lookup failed (continuing)", slog.String("user_id", userID))
	}
	if email == "" {
		email = info.Email
	}

	res, err := o.provisioner.ProvisionTeslaOwner(ctx, store.ProvisionInput{
		UserID:            userID,
		ProviderAccountID: info.Sub,
		Name:              name,
		Email:             email,
		AccessToken:       accessToken,
		RefreshToken:      refreshToken,
		ExpiresAt:         expiresAt,
	})
	if err != nil {
		return err
	}

	o.logger.Info("owner provisioned + tesla linked",
		slog.String("caller_id", userID),
		slog.String("user_id", res.CanonicalUserID),
		slog.String("outcome", string(res.Outcome)))

	if o.hook != nil {
		// Best-effort: never block or fail the link on stream setup. Vehicles are
		// owned under the CANONICAL user (a converged link may differ from the
		// caller's original id), so pass res.CanonicalUserID, not userID.
		o.hook.AfterLink(ctx, res.CanonicalUserID, accessToken)
	}
	return nil
}
