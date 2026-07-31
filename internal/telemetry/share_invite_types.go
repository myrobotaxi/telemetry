package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// Typed failures the sharing store surfaces to the handler layer. They are
// EXPORTED because the adapter in cmd/telemetry-server translates the
// equivalent internal/store sentinels into them at the boundary —
// internal/telemetry does not import internal/store.
//
// None of them wraps sdk.ErrNotFound: each maps to a distinct non-404 status.
// "Not found" itself travels as sdk.ErrNotFound, which is what an invalid,
// expired, or foreign invite must be indistinguishable from.
var (
	// ErrShareVehicleNotOwned reports that some vehicle in a create request
	// is not owned by the caller. → 403 permission_denied.
	ErrShareVehicleNotOwned = errors.New("vehicle not owned by inviting user")

	// ErrShareNotPending reports a resend against an already-accepted
	// grant. → 409 conflict.
	ErrShareNotPending = errors.New("share invite is not pending")

	// ErrShareSelfRedeem reports that the redeeming caller owns one of the
	// target vehicles. → 409 conflict, with nothing accepted.
	ErrShareSelfRedeem = errors.New("cannot redeem an invite for your own vehicle")

	// ErrShareAlreadyGranted reports that the redeemer already holds a grant
	// on one of the target vehicles through a different invite.
	// → 409 conflict.
	ErrShareAlreadyGranted = errors.New("vehicle already shared with this user")
)

// Wire shapes and dependency interfaces for the MYR-184 vehicle-sharing REST
// surface (rest-api.md §7.5, schemas/vehicle-sharing.schema.json).
//
// Two response objects exist and they are deliberately asymmetric:
//
//   - shareInviteWire is OWNER-FACING. It carries the owner-typed `label` and,
//     while pending, the live `code`. It is never returned to an invited party.
//   - redeemResponse is what the INVITED PARTY sees. It carries the owner's
//     first name and the granted vehicles, and nothing that would leak the
//     owner's label or a still-live code.
//
// Mixing them up is the single worst mistake available on this surface, which
// is why they live in separate types with no shared serializer.

// ShareInviteRow is the slim invite projection the handlers consume from their
// store dependency. Mirrors store.VehicleShare minus the server-only fields
// (accepted_by_user_id, revoked_at) the wire never carries — defined here so
// internal/telemetry stays decoupled from internal/store.
type ShareInviteRow struct {
	ID         string
	VehicleID  string
	Label      string
	Permission string
	// Code is empty for any row that is not pending. P1 bearer credential —
	// never logged, never in an error envelope.
	Code       string
	Status     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	AcceptedAt *time.Time
}

// ShareGrantRow is what a redemption granted, per vehicle.
type ShareGrantRow struct {
	VehicleID   string
	OwnerUserID string
	Permission  string
}

// ShareInviteStore is the owner-side dependency: mint, list, revoke, resend.
// Defined at the consumer site per the dependency rule; the adapter in
// cmd/telemetry-server binds store.VehicleShareRepo to it.
type ShareInviteStore interface {
	// CreateInvite mints one code across every vehicle in the set and
	// returns the row for pathVehicleID.
	CreateInvite(ctx context.Context, in ShareInviteCreateInput) (ShareInviteRow, error)
	// ListInvitesForVehicle returns the owner's pending invites and
	// accepted grants for one vehicle, newest first. Revoked rows excluded.
	ListInvitesForVehicle(ctx context.Context, vehicleID, ownerUserID string) ([]ShareInviteRow, error)
	// RevokeInvite tombstones a row. Idempotent. The returned user id is
	// whoever lost access (empty if the row was still pending), for cache
	// invalidation.
	RevokeInvite(ctx context.Context, inviteID, ownerUserID string) (string, error)
	// ResendInvite re-mints the code and resets the expiry on a pending row.
	ResendInvite(ctx context.Context, inviteID, ownerUserID string) (ShareInviteRow, error)
	// OwnerFirstName resolves the calling owner's first name — the `from`
	// half of the signed share link (MYR-368). Same method, same ladder, and
	// same P1 first-names-only policy as the redeem side; it is listed on
	// this interface too because the owner surface now needs it as well.
	// Never returns an empty string.
	OwnerFirstName(ctx context.Context, ownerUserID string) (string, error)
}

// ShareRedeemStore is the rider-side dependency.
type ShareRedeemStore interface {
	// RedeemCode accepts every pending unexpired row for a code atomically.
	RedeemCode(ctx context.Context, code, redeemerID string) ([]ShareGrantRow, error)
	// OwnerFirstName resolves the sharing owner's first name for the
	// redeemer's success screen. Never returns an empty string.
	OwnerFirstName(ctx context.Context, ownerUserID string) (string, error)
}

// ShareInviteCreateInput is the validated create request.
type ShareInviteCreateInput struct {
	OwnerUserID   string
	PathVehicleID string
	VehicleIDs    []string
	Label         string
	Permission    string
}

// AccessCacheInvalidator lets the sharing handlers bust a user's cached
// vehicle access set the moment it changes. Satisfied by
// *auth.JWTAuthenticator. Optional everywhere — a nil invalidator means the
// change becomes visible on the next cache expiry instead of immediately.
type AccessCacheInvalidator interface {
	InvalidateVehicles(userID string)
}

// createShareInviteRequest is the POST /api/vehicles/{vehicleId}/invites body.
// Strict per the schema: owner, id, status, code, and both timestamps are all
// server-assigned and are not accepted from a client.
type createShareInviteRequest struct {
	Label      string   `json:"label"`
	Permission string   `json:"permission"`
	VehicleIDs []string `json:"vehicleIds"`
}

// redeemShareInviteRequest is the POST /api/invites/redeem body.
type redeemShareInviteRequest struct {
	Code string `json:"code"`
}

// shareInviteWire is the owner-facing ShareInvite object.
//
// Four fields are conditionally OMITTED, and the omission is contractual, not
// cosmetic: `code`, `shareUrl` and `expiresAt` exist only while the row is
// pending (an accepted grant's code is not re-readable and an accepted grant
// does not expire), and `acceptedAt` exists only once it has been accepted.
type shareInviteWire struct {
	InviteID   string `json:"inviteId"`
	VehicleID  string `json:"vehicleId"`
	Label      string `json:"label"`
	Permission string `json:"permission"`
	Status     string `json:"status"`
	Code       string `json:"code,omitempty"`
	// ShareURL is the signed join link (MYR-368). It CONTAINS the code, so
	// it is P1 and bearer exactly as Code is: never logged, never echoed
	// into an error. Empty when no signing key is configured, in which case
	// the key is omitted and a consumer falls back to sharing Code.
	ShareURL   string `json:"shareUrl,omitempty"`
	CreatedAt  string `json:"createdAt"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
	AcceptedAt string `json:"acceptedAt,omitempty"`
}

// shareInviteListResponse is the GET envelope. The key is `invites`, NOT the
// `items` used by the cursor-paginated list envelopes: this surface is
// deliberately unpaginated and the distinct key keeps an SDK pagination helper
// from mistaking it for a page.
type shareInviteListResponse struct {
	Invites []map[string]any `json:"invites"`
}

// redeemShareInviteResponse is the invited party's 200 body: everything the
// join-success screen needs without a second round trip.
type redeemShareInviteResponse struct {
	OwnerFirstName string           `json:"ownerFirstName"`
	Vehicles       []map[string]any `json:"vehicles"`
}

// toShareInviteWire projects a row onto the wire shape, applying the
// conditional omissions. signer may be nil, in which case shareUrl is omitted.
//
// The code is emitted ONLY for a pending row. The store already blanks it for
// non-pending rows in SQL; this is the second, independent gate, because a
// leaked accepted-grant code is a live credential handed to whoever can list
// the invite. `shareUrl` is minted inside the SAME branch and from the SAME
// two values, so the link cannot outlive the code it embeds — an accepted row
// that somehow carried a URL would leak the credential the code branch just
// withheld.
func toShareInviteWire(row *ShareInviteRow, link inviteLinkCtx) shareInviteWire {
	out := shareInviteWire{
		InviteID:   row.ID,
		VehicleID:  row.VehicleID,
		Label:      row.Label,
		Permission: row.Permission,
		Status:     row.Status,
		CreatedAt:  row.CreatedAt.UTC().Format(time.RFC3339),
	}
	if row.Status == shareStatusPending {
		out.Code = row.Code
		if !row.ExpiresAt.IsZero() {
			out.ExpiresAt = row.ExpiresAt.UTC().Format(time.RFC3339)
		}
		// Signed over the row's ACTUAL expires_at, so the instant in
		// the link is the instant redemption enforces.
		out.ShareURL = link.signer.ShareURL(row.Code, row.ExpiresAt, link.ownerName, row.Label)
	}
	if row.AcceptedAt != nil {
		out.AcceptedAt = row.AcceptedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// toShareInviteMasked projects a row onto the wire AND through the
// ResourceInvite owner allow-list (internal/mask/tables.go inviteOwnerFields,
// rest-api.md §5.2.5).
//
// Running the mask on a surface that is already owner-gated at the router looks
// redundant, and that is the point: it makes the allow-list LOAD-BEARING. A
// field added to shareInviteWire without a matching entry in the table is
// dropped from the response instead of shipping unclassified — which is exactly
// the drift that left the table describing a Prisma `Invite` shape this server
// never served. The viewer role has no entry for this resource, so if a viewer
// ever reached this code the same call would return an empty object rather than
// an owner's labels and a live code.
func toShareInviteMasked(row *ShareInviteRow, role auth.Role, link inviteLinkCtx) map[string]any {
	wire := toShareInviteWire(row, link)
	fields := map[string]any{
		"inviteId":   wire.InviteID,
		"vehicleId":  wire.VehicleID,
		"label":      wire.Label,
		"permission": wire.Permission,
		"status":     wire.Status,
		"createdAt":  wire.CreatedAt,
	}
	// The four conditional keys are added only when they have a value, so
	// omission stays omission rather than becoming an empty string.
	if wire.Code != "" {
		fields["code"] = wire.Code
	}
	if wire.ShareURL != "" {
		fields["shareUrl"] = wire.ShareURL
	}
	if wire.ExpiresAt != "" {
		fields["expiresAt"] = wire.ExpiresAt
	}
	if wire.AcceptedAt != "" {
		fields["acceptedAt"] = wire.AcceptedAt
	}
	projected, _ := mask.Apply(fields, mask.For(mask.ResourceInvite, role))
	return projected
}

// Wire-visible statuses. 'revoked' is deliberately absent: revoked rows are
// server-side tombstones and are never serialized.
const (
	shareStatusPending  = "pending"
	shareStatusAccepted = "accepted"
)

// maxShareInviteLabelLen bounds the owner-typed recipient name. Generous
// enough for any real name, small enough that the field cannot be used as
// free storage.
const maxShareInviteLabelLen = 120

// maxShareInviteVehicles bounds a single multi-vehicle invite. One code
// granting an unbounded set would make one leaked code unboundedly costly.
const maxShareInviteVehicles = 20

// validSharePermission reports whether p is one of the three tiers, using the
// auth package's parser so there is exactly one enum in the process.
func validSharePermission(p string) bool {
	_, err := auth.ParseSharePermission(p)
	return err == nil
}
