package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
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

	// ErrShareNotAccepted reports a PATCH against an invite that is still
	// pending (MYR-369). → 409 conflict. The mirror image of
	// ErrShareNotPending: a pending row has no grant to edit, because its
	// access is decided at redemption from the invite's preset.
	ErrShareNotAccepted = errors.New("share invite is not an accepted grant")

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
	ID        string
	VehicleID string
	Label     string
	// Permission is the INVITE-TIME PRESET as stored (MYR-369). It is what
	// a PENDING row serializes; an ACCEPTED row's wire `permission` is
	// DERIVED from Grant instead, so this field is not read there.
	Permission string
	// Grant is the accepted grant's capability set. Meaningless on a pending
	// row, where the wire omits both flags — the zero value is the
	// fail-closed reading if anything looks anyway.
	Grant auth.ShareGrant
	// Code is empty for any row that is not pending. P1 bearer credential —
	// never logged, never in an error envelope.
	Code       string
	Status     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	AcceptedAt *time.Time
}

// ShareGrantRow is what a redemption granted, per vehicle — the FLAGS it wrote,
// not the preset that produced them (MYR-369).
type ShareGrantRow struct {
	VehicleID   string
	OwnerUserID string
	AllowRides  bool
}

// ShareInvitePatch is one owner edit of one accepted grant. Both fields are
// POINTERS because absent and false are different requests: nil means "leave
// this capability alone", non-nil false means "take it away". A bare bool would
// make every partial patch silently clear the field it did not mention.
type ShareInvitePatch struct {
	AllowRides *bool
	Suspended  *bool
}

// HasChange reports whether the patch asks for anything at all.
func (p ShareInvitePatch) HasChange() bool {
	return p.AllowRides != nil || p.Suspended != nil
}

// ShareLeaveOutcome is what a rider's leave found (MYR-469).
type ShareLeaveOutcome int

const (
	// ShareLeaveDone — the caller no longer holds an accepted grant on the
	// vehicle, whether this call removed one or there was nothing to remove.
	ShareLeaveDone ShareLeaveOutcome = iota
	// ShareLeaveRefusedLiveRide — a live ride holds the grant in place.
	ShareLeaveRefusedLiveRide
)

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
	// RevokeInvite tombstones a row. Idempotent. The returned RevokedGrant
	// names whoever lost access and the vehicle they lost — for cache
	// invalidation and for closing their live socket. Its zero value means
	// the call removed nothing (a pending row, or an already-revoked one).
	RevokeInvite(ctx context.Context, inviteID, ownerUserID string) (RevokedGrant, error)
	// ResendInvite re-mints the code and resets the expiry on a pending row.
	ResendInvite(ctx context.Context, inviteID, ownerUserID string) (ShareInviteRow, error)
	// LeaveVehicleShares tombstones every accepted grant the CALLER redeemed
	// on this vehicle (MYR-469 — the rider-side mirror of RevokeInvite).
	// Idempotent; refused (without writing) while the caller has a live ride
	// on the car, because the ride's telemetry access rides the grant.
	LeaveVehicleShares(ctx context.Context, vehicleID, viewerUserID string) (ShareLeaveOutcome, error)
	// PatchInvite applies an owner edit to one ACCEPTED grant and returns
	// the row as it now stands (MYR-369). The returned user id is the
	// grantee whose access set changed, for cache invalidation — always
	// populated on success, because an accepted grant always has one.
	//
	// Errors: sdk.ErrNotFound for a row that is missing, foreign or
	// revoked (indistinguishably); ErrShareNotAccepted for one that is the
	// caller's but still pending.
	PatchInvite(ctx context.Context, inviteID, ownerUserID string, patch ShareInvitePatch) (ShareInviteRow, string, error)
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

// RevokedGrant is what a revocation removed: the grantee who lost access and
// the vehicle they lost it on. The zero value means the call removed nothing —
// a still-pending invite nobody had redeemed, or the idempotent second DELETE
// of an already-revoked row. Callers MUST check ViewerUserID before acting on
// it; an empty id is not a wildcard.
type RevokedGrant struct {
	ViewerUserID string
	VehicleID    string
}

// AccessCacheInvalidator lets the sharing handlers bust a user's cached
// vehicle access set the moment it changes. Satisfied by
// *auth.JWTAuthenticator. Optional everywhere — a nil invalidator means the
// change becomes visible on the next cache expiry instead of immediately.
type AccessCacheInvalidator interface {
	InvalidateVehicles(userID string)
}

// ShareAccessNotifier announces that a grantee has LOST access to a vehicle,
// so a live WebSocket carrying that car's telemetry to them can be torn down
// now instead of at the next reconnect (MYR-373, closing
// websocket-protocol.md §10 DV-09).
//
// Busting the cache is not enough on its own and that is the whole point of
// this interface. The cache is what the HANDSHAKE consults; a socket that
// already completed its handshake never consults anything again, because the
// access set is frozen on the Client at that moment. So suspend and revoke
// need two signals: one to stop the next connection (the invalidator above)
// and one to end the current one (this).
//
// Defined at the consumer site and deliberately primitive-valued so this
// package does not take a dependency on internal/events. The adapter in
// cmd/telemetry-server publishes an events.ShareAccessRevokedEvent; the hub's
// dispatcher does the closing.
//
// Optional, like the invalidator: a nil notifier leaves the pre-MYR-373
// behavior in place — the grant is gone everywhere except an already-open
// socket, which lapses at reconnect or at the revalidation backstop.
//
// MUST NOT BLOCK. It is called on the request path with the owner waiting on
// their 200; implementations publish to an in-process bus and return.
type ShareAccessNotifier interface {
	// ShareAccessRevoked announces that granteeUserID has lost access to
	// vehicleID. reason is "revoked" or "suspended" and is for the server
	// log only — a viewer is never told which lever the owner pulled.
	ShareAccessRevoked(granteeUserID, vehicleID, reason string)
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

// patchShareInviteRequest is the PATCH /api/invites/{inviteId} body
// (vehicle-sharing.schema.json PatchShareInviteRequest).
//
// POINTER FIELDS, deliberately. encoding/json leaves a pointer nil when the key
// is ABSENT and sets it to a pointer-to-false when the key is present with
// `false`, which is precisely the distinction the partial semantics need. Plain
// bools would collapse both onto false and make every patch that mentioned one
// field silently revoke the other.
type patchShareInviteRequest struct {
	AllowRides *bool `json:"allowRides"`
	Suspended  *bool `json:"suspended"`
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
