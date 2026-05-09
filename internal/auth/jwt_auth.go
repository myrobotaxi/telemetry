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

// queryUserVehicleIDs fetches all vehicle IDs belonging to a user from
// the Prisma-owned "Vehicle" table.
const queryUserVehicleIDs = `SELECT "id" FROM "Vehicle" WHERE "userId" = $1`

// queryUserExists is a slim row-existence probe against the
// Prisma-owned "User" table, used by the FR-10.1 fail-closed JWT
// existence check (data-lifecycle.md §3.5, MYR-73). SELECT 1 + LIMIT
// 1 lets Postgres short-circuit on the primary-key index without
// pulling any data columns.
const queryUserExists = `SELECT 1 FROM "User" WHERE "id" = $1 LIMIT 1`

// queryVehicleOwnerByID fetches the owning user ID for a vehicle. Used
// by ResolveRole to determine whether the caller is the owner of the
// vehicle or a viewer (post-MYR-Invite, after the FR-5.4 invite path
// lands; today the only path to the viewer branch is a stale cache).
const queryVehicleOwnerByID = `SELECT "userId" FROM "Vehicle" WHERE "id" = $1`

// JWTAuthenticator validates HS256 JWTs and resolves the authenticated
// user's vehicle IDs from the database. It caches vehicle lookups to
// avoid hitting the DB on every WebSocket reconnect.
type JWTAuthenticator struct {
	secret           []byte
	issuer           string
	audience         string
	cache            *vehicleCache
	ownerLookup      vehicleOwnerLookup
	userExistsCache  *userExistenceCache
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

// NewJWTAuthenticator creates an authenticator that verifies HS256 JWTs
// using the given secret and queries the pool for vehicle ownership.
// Issuer and audience are validated if non-empty.
func NewJWTAuthenticator(secret, issuer, audience string, pool *pgxpool.Pool) *JWTAuthenticator {
	querier := &pgVehicleQuerier{pool: pool}
	existenceQuerier := &pgUserExistenceQuerier{pool: pool}
	return &JWTAuthenticator{
		secret:          []byte(secret),
		issuer:          issuer,
		audience:        audience,
		cache:           newVehicleCache(querier, vehicleCacheTTL),
		ownerLookup:     querier,
		userExistsCache: newUserExistenceCache(existenceQuerier, userExistenceTTL),
	}
}

// ValidateToken parses and verifies an HS256 JWT, checks expiration, and
// returns the user ID from the "sub" claim.
func (a *JWTAuthenticator) ValidateToken(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("auth.ValidateToken: %w", ErrInvalidToken)
	}

	opts := []jwt.ParserOption{jwt.WithValidMethods([]string{"HS256"})}
	if a.issuer != "" {
		opts = append(opts, jwt.WithIssuer(a.issuer))
	}
	if a.audience != "" {
		opts = append(opts, jwt.WithAudience(a.audience))
	}

	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return a.secret, nil
	}, opts...)

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
// The query reads "Vehicle"."userId" by primary key. If the row's
// userId matches the caller, the caller is the owner. Otherwise the
// caller is treated as a viewer. Vehicle-not-found is surfaced as an
// error so the caller can convert it to 404 / fail-closed at the
// handler layer; an unknown vehicle MUST NOT be silently downgraded to
// "viewer" because that would leak the vehicle's existence.
func (a *JWTAuthenticator) ResolveRole(ctx context.Context, userID, vehicleID string) (Role, error) {
	ownerID, err := a.ownerLookup.GetVehicleOwnerByID(ctx, vehicleID)
	if err != nil {
		return Role(""), fmt.Errorf("ResolveRole: vehicle %s not found: %w", vehicleID, err)
	}
	if ownerID == userID {
		return RoleOwner, nil
	}
	// TODO: when Invite-based viewer access lands, query the Invite table
	// (Prisma-owned, see data-classification.md §1.6) to verify this user has
	// an accepted invite granting viewer access to vehicleID. Until then,
	// returning RoleViewer is forward-looking — the only path to reach this
	// branch today is a stale GetUserVehicles cache or a test fixture.
	return RoleViewer, nil
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
