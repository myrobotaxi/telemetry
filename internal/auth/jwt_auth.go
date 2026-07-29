// Package auth provides JWT-based authentication for WebSocket clients.
// It implements the ws.Authenticator interface using HS256 JWTs signed by
// the same AUTH_SECRET the Next.js frontend uses.
package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors returned by JWTAuthenticator.
var (
	// ErrInvalidToken indicates the JWT is malformed, expired, or signed
	// with the wrong key.
	ErrInvalidToken = errors.New("invalid token")

	// ErrMissingSubject indicates the JWT has no "sub" claim.
	ErrMissingSubject = errors.New("missing subject claim")
)

// queryUserVehicleIDs fetches the caller's full VEHICLE ACCESS SET — the
// vehicles they own, UNIONed with the vehicles somebody has shared with them
// (MYR-91 viewer merge / MYR-184 sharing).
//
// Before MYR-184 the access set was ownership alone, which is why the `viewer`
// role existed in the mask matrix without a single row that could produce it.
// The second leg is what makes it real: an accepted go_vehicle_shares grant
// (Go-owned, migration 0020) puts the vehicle in the grantee's set, so it flows
// through EVERY consumer of this query at once — the WebSocket subscribed set,
// GET /api/vehicles, and every per-vehicle handler that asks "can this caller
// see this car".
//
// Revoked and pending rows are excluded by the `status = 'accepted'` predicate:
// an invite that was never redeemed grants nothing, and revocation is a
// tombstone flip, so a revoked grant drops out of the set on the next lookup.
// UNION (not UNION ALL) de-duplicates the case where an owner also holds a
// stale grant row for their own car.
const queryUserVehicleIDs = `
SELECT "id" FROM "Vehicle" WHERE "userId" = $1
UNION
SELECT vehicle_id FROM go_vehicle_shares
WHERE accepted_by_user_id = $1 AND status = 'accepted'`

// queryUserExists is a slim row-existence probe used by the FR-10.1
// fail-closed JWT existence check (data-lifecycle.md §3.5, MYR-73). A user
// is "live" if a row exists in EITHER the Prisma-owned "User" table (web /
// Google users) OR the Go-owned go_users table (Apple-native users minted by
// the identity module, MYR-193 — they have no legacy Prisma row). Both EXISTS
// sub-probes hit a primary-key index, so this stays a microsecond lookup.
// The query returns one row when the user exists and zero rows otherwise, so
// UserExists's pgx.ErrNoRows handling is unchanged.
const queryUserExists = `SELECT 1 WHERE EXISTS (SELECT 1 FROM "User" WHERE "id" = $1) OR EXISTS (SELECT 1 FROM go_users WHERE "id" = $1)`

// queryVehicleOwnerByID fetches the owning user ID for a vehicle. Used
// by ResolveRole to determine whether the caller is the owner of the
// vehicle or a viewer (post-MYR-Invite, after the FR-5.4 invite path
// lands; today the only path to the viewer branch is a stale cache).
const queryVehicleOwnerByID = `SELECT "userId" FROM "Vehicle" WHERE "id" = $1`

// JWTAuthenticator validates HS256 JWTs and resolves the authenticated
// user's vehicle IDs from the database. It caches vehicle lookups to
// avoid hitting the DB on every WebSocket reconnect.
type JWTAuthenticator struct {
	secret          []byte
	issuer          string
	audience        string
	cache           *vehicleCache
	ownerLookup     vehicleOwnerLookup
	userExistsCache *userExistenceCache
	// shares resolves accepted vehicle-sharing grants (MYR-184). Nil means
	// no share lookup is configured, in which case ResolveVehicleAccess
	// fails closed and viewer is unreachable.
	shares shareLookup
	// es256 optionally verifies ES256 tokens minted by the identity module
	// (ADR-001 §3). Nil => HS256-only (legacy behaviour, unchanged).
	es256 ES256KeyResolver
}

// Compile-time interface check.
var _ wsAuthenticator = (*JWTAuthenticator)(nil)

// wsAuthenticator mirrors the ws.Authenticator interface to avoid an
// import cycle. If ws.Authenticator changes, main.go (which assigns
// *JWTAuthenticator to ws.Authenticator) will fail at compile time.
type wsAuthenticator interface {
	ValidateToken(ctx context.Context, token string) (string, error)
	GetUserVehicles(ctx context.Context, userID string) ([]string, error)
	ResolveRole(ctx context.Context, userID, vehicleID string) (Role, error)
}

// vehicleOwnerLookup is the consumer-site interface used by ResolveRole
// to fetch a vehicle's owning user ID. Defined here so tests can swap
// the DB-backed implementation for a stub.
type vehicleOwnerLookup interface {
	GetVehicleOwnerByID(ctx context.Context, vehicleID string) (ownerID string, err error)
}

// NewJWTAuthenticator creates an authenticator that verifies JWTs using the
// given HS256 secret and queries the pool for vehicle ownership. Issuer and
// audience are validated if non-empty. Pass WithES256Resolver to additionally
// accept ES256 tokens minted by the identity module (ADR-001 §3); without it
// the validator is HS256-only and unchanged.
func NewJWTAuthenticator(secret, issuer, audience string, pool *pgxpool.Pool, opts ...Option) *JWTAuthenticator {
	querier := &pgVehicleQuerier{pool: pool}
	existenceQuerier := &pgUserExistenceQuerier{pool: pool}
	a := &JWTAuthenticator{
		secret:          []byte(secret),
		issuer:          issuer,
		audience:        audience,
		cache:           newVehicleCache(querier, vehicleCacheTTL),
		ownerLookup:     querier,
		userExistsCache: newUserExistenceCache(existenceQuerier, userExistenceTTL),
		// The same querier serves the accepted-share lookup (MYR-184), so
		// the DB-backed authenticator resolves viewers out of the box —
		// nothing has to remember to opt in.
		shares: querier,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// ValidateToken parses and verifies a JWT — legacy HS256 (shared secret) or,
// when an ES256 resolver is configured, ES256 (identity-module keypair,
// resolved by `kid`) — with the algorithm pinned per token (ADR-001 §3). It
// checks issuer/audience/expiry and returns the user ID from the "sub" claim.
func (a *JWTAuthenticator) ValidateToken(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("auth.ValidateToken: %w", ErrInvalidToken)
	}

	opts := []jwt.ParserOption{jwt.WithValidMethods(a.validMethods())}
	if a.issuer != "" {
		opts = append(opts, jwt.WithIssuer(a.issuer))
	}
	if a.audience != "" {
		opts = append(opts, jwt.WithAudience(a.audience))
	}

	parsed, err := jwt.Parse(token, a.keyFunc, opts...)

	if err != nil {
		return "", fmt.Errorf("auth.ValidateToken: %w: %w", ErrInvalidToken, err)
	}

	sub, err := parsed.Claims.GetSubject()
	if err != nil || sub == "" {
		return "", fmt.Errorf("auth.ValidateToken: %w", ErrMissingSubject)
	}

	// FR-10.1 fail-closed existence check (data-lifecycle.md §3.5,
	// MYR-73): a token whose user has been deleted MUST be rejected.
	// Authoritative state lives in the User table; the cache absorbs
	// the per-frame DB cost via singleflight + 1s TTL. A deleted
	// user's stale token may remain valid for at most ttl after
	// deletion (acceptable per the issue spec — the WS hub close path
	// also fires within seconds via LISTEN/NOTIFY).
	if a.userExistsCache != nil {
		exists, existsErr := a.userExistsCache.Exists(ctx, sub)
		if existsErr != nil {
			// Fail-closed wrap: callers that branch on
			// errors.Is(err, ErrUserNotFound) get the same answer in
			// the "DB outage" branch as in the "row missing" branch.
			// The underlying transient error stays in the chain for
			// observability.
			return "", fmt.Errorf("auth.ValidateToken: %w: %w: %w", ErrInvalidToken, ErrUserNotFound, existsErr)
		}
		if !exists {
			return "", fmt.Errorf("auth.ValidateToken: %w: %w", ErrInvalidToken, ErrUserNotFound)
		}
	}

	return sub, nil
}

// GetUserVehicles returns the vehicle IDs (Prisma cuids) that the user is
// authorized to receive telemetry for. Results are cached for 5 minutes.
func (a *JWTAuthenticator) GetUserVehicles(ctx context.Context, userID string) ([]string, error) {
	ids, err := a.cache.lookup(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("auth.GetUserVehicles(user=%s): %w", userID, err)
	}
	return ids, nil
}

// ResolveRole returns the caller's role (owner | viewer) for the given
// vehicle. Used by both the WebSocket per-role projection
// (websocket-protocol.md §4.6) and the REST handler-layer mask
// (rest-api.md §5.1).
//
// MYR-184 CLOSED THE FAIL-OPEN HOLE THAT USED TO LIVE HERE. This function
// previously returned RoleViewer for ANY caller who was not the owner, on the
// grounds that the only way to reach the branch was a stale cache. That is no
// longer a safe assumption now that viewer is a role real callers hold: a
// non-owner with no accepted share gets ErrNoVehicleAccess, and callers convert
// that to 403 / deny-all rather than to a viewer projection.
//
// See ResolveVehicleAccess (vehicle_access.go) for the tier-carrying form that
// handlers gating on live_history / rides need.
func (a *JWTAuthenticator) ResolveRole(ctx context.Context, userID, vehicleID string) (Role, error) {
	role, _, err := a.ResolveVehicleAccess(ctx, userID, vehicleID)
	return role, err
}

// pgVehicleQuerier queries PostgreSQL for a user's vehicle IDs.
type pgVehicleQuerier struct {
	pool *pgxpool.Pool
}

// GetUserVehicleIDs queries the "Vehicle" table for all vehicles belonging
// to the given user.
func (q *pgVehicleQuerier) GetUserVehicleIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := q.pool.Query(ctx, queryUserVehicleIDs, userID)
	if err != nil {
		return nil, fmt.Errorf("pgVehicleQuerier.GetUserVehicleIDs: query: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("pgVehicleQuerier.GetUserVehicleIDs: scan: %w", err)
		}
		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgVehicleQuerier.GetUserVehicleIDs: rows: %w", err)
	}

	return ids, nil
}

// GetVehicleOwnerByID returns the owning user ID for a single vehicle
// row, looked up by primary key. The query targets the Prisma-owned
// "Vehicle" table.
func (q *pgVehicleQuerier) GetVehicleOwnerByID(ctx context.Context, vehicleID string) (string, error) {
	var ownerID string
	if err := q.pool.QueryRow(ctx, queryVehicleOwnerByID, vehicleID).Scan(&ownerID); err != nil {
		return "", fmt.Errorf("pgVehicleQuerier.GetVehicleOwnerByID(%s): %w", vehicleID, err)
	}
	return ownerID, nil
}

// InvalidateUser drops the cached existence entry for userID so the
// next ValidateToken call refetches from the database. Called by the
// VehicleDeletedEvent handler (data-lifecycle.md §3.5) so a deleted
// user's tokens are rejected immediately instead of after the 1s TTL.
//
// The §3.5 trigger fires per-Vehicle, not per-User, so the same
// userID can be invalidated multiple times in a row when several
// vehicles are deleted in one transaction; Invalidate is idempotent.
func (a *JWTAuthenticator) InvalidateUser(userID string) {
	if a.userExistsCache == nil {
		return
	}
	a.userExistsCache.Invalidate(userID)
}

// pgUserExistenceQuerier queries the Prisma-owned "User" table for a
// row's existence. Used by userExistenceCache to keep the FR-10.1
// fail-closed check off the hot path.
type pgUserExistenceQuerier struct {
	pool *pgxpool.Pool
}

// UserExists returns true when a User row with the given id exists.
// Returns false (no error) for "no row"; returns an error only on
// transient transport problems. The query targets the primary-key
// index so this is a microsecond-cost lookup even at scale.
func (q *pgUserExistenceQuerier) UserExists(ctx context.Context, userID string) (bool, error) {
	var one int
	err := q.pool.QueryRow(ctx, queryUserExists, userID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("pgUserExistenceQuerier.UserExists(%s): %w", userID, err)
}
