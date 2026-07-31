package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// Code lifetime is SEVEN DAYS, and it lives in SQL rather than here — see
// shareInviteTTLInterval in vehicle_share_queries.go. Both the value written to
// expires_at and the `expires_at > NOW()` predicate that reads it are evaluated
// by the database, so "expired" never depends on the app server's clock
// agreeing with the database's.

// go_vehicle_shares (migration 0020, MYR-184) is the vehicle-sharing grant
// table: one row per (owner → recipient → vehicle) grant, created as a pending
// invite carrying a redeemable code and flipped to an accepted viewer grant
// when somebody redeems it. This file holds the shapes the repository speaks;
// the SQL lives in vehicle_share_queries.go.
//
// Two fields are P1 (data-classification.md §1.15). `Label` is a person's
// name; `Code` is a live bearer credential for vehicle access. NEITHER is ever
// logged — every log line and every error string in this package identifies a
// row by its `ID` instead. Code is additionally blanked on the way out of the
// repository for any row that is no longer pending.

// Share statuses as persisted. 'revoked' is a server-side tombstone and never
// reaches the wire.
const (
	ShareStatusPending  = "pending"
	ShareStatusAccepted = "accepted"
	ShareStatusRevoked  = "revoked"
)

// Share permission PRESETS. No longer a cumulative tier (MYR-369): the accepted
// grant's access lives in the allow_rides / suspended_at columns migration 0024
// added, and this value survives only as the invite-time preset redemption maps
// onto those flags. See internal/auth.SharePermission for the full note.
//
// SharePermissionLiveHistory is RETIRED — accepted at create time so an
// un-updated client keeps working, normalized to SharePermissionLive before it
// is written, and never emitted.
const (
	SharePermissionLive        = "live"
	SharePermissionLiveHistory = "live_history"
	SharePermissionRides       = "rides"
)

var (
	// ErrShareNotFound is returned when an invite lookup, or a conditional
	// update guarded on ownership, matches no row. Wraps sdk.ErrNotFound.
	//
	// It is also what an invalid, expired, or already-consumed redeem code
	// produces: the redeem surface answers 404 for all three
	// INDISTINGUISHABLY so an enumerating caller learns nothing about which
	// case it hit.
	ErrShareNotFound = fmt.Errorf("vehicle share %w", sdk.ErrNotFound)

	// ErrShareNotPending is returned by Resend when the row exists and
	// belongs to the caller but has already been accepted — a resend would
	// have to un-accept a live grant, which is a revoke plus a new invite,
	// not a resend. The HTTP layer maps it to 409 conflict. Deliberately
	// does NOT wrap sdk.ErrNotFound.
	ErrShareNotPending = errors.New("vehicle share is not pending")

	// ErrShareSelfRedeem is returned when the redeeming caller owns one of
	// the vehicles the code grants. You cannot become a viewer of a car you
	// already own; the whole redemption is refused so nothing is partially
	// accepted. The HTTP layer maps it to 409 conflict.
	ErrShareSelfRedeem = errors.New("cannot redeem an invite for your own vehicle")

	// ErrShareAlreadyGranted is returned when the redeemer already holds an
	// accepted grant for one of the target vehicles through a DIFFERENT
	// invite — the partial-unique accepted-grant index refuses the second
	// row. The HTTP layer maps it to 409 conflict. (A repeat of the SAME
	// code by the SAME person is not this: that is the idempotent re-redeem
	// path and returns 200.)
	ErrShareAlreadyGranted = errors.New("vehicle already shared with this user")

	// ErrShareVehicleNotOwned is returned by CreateInvite when any requested
	// vehicle is not owned by the caller. The HTTP layer maps it to 403.
	ErrShareVehicleNotOwned = errors.New("vehicle not owned by inviting user")

	// ErrShareNotAccepted is returned by PatchInvite when the row exists and
	// belongs to the caller but is still PENDING (MYR-369). A pending invite
	// has no grant to edit — its access is decided at redemption from the
	// invite's preset — so the owner cancels and re-sends rather than
	// patching. The HTTP layer maps it to 409 conflict.
	//
	// Deliberately distinct from ErrShareNotPending, which is the opposite
	// complaint from Resend, and deliberately NOT wrapping sdk.ErrNotFound:
	// the caller demonstrably owns this row, so hiding its existence would
	// tell them nothing they do not already know while costing them the
	// reason their edit failed.
	ErrShareNotAccepted = errors.New("vehicle share is not an accepted grant")

	// ErrShareCodeCollision is returned when a single code resolves to
	// pending rows belonging to MORE THAN ONE owner. The code space is
	// 36^6, so this is astronomically unlikely, but redeeming such a code
	// would silently grant a viewer access to two unrelated people's cars.
	// The redemption is refused rather than guessed at.
	ErrShareCodeCollision = errors.New("vehicle share code collision")
)

// VehicleShare is one grant row as the repository returns it.
//
// Code is blanked for any row whose Status is not pending: an accepted grant's
// code is not re-readable, which is the contract's own rule
// (ShareInvite.code — "OMITTED on every accepted row"). AcceptedByUserID is
// server-side only and is never serialized on the owner-facing wire shape.
type VehicleShare struct {
	ID          string
	VehicleID   string
	OwnerUserID string
	// Label is the owner-typed recipient name. P1 — never logged.
	Label string
	// Permission is the INVITE-TIME PRESET, not the grant's access
	// (MYR-369). Redemption maps it onto AllowRides; nothing else reads it.
	Permission string
	// AllowRides is the per-grant, owner-editable ride capability
	// (migration 0024). Meaningful on an accepted row; inert on a pending
	// one, where it merely mirrors what the preset will produce.
	AllowRides bool
	// SuspendedAt is non-nil while the owner has PAUSED this grant. A
	// suspended grant conveys nothing at all — it is excluded from the
	// viewer access set — so this field out-votes AllowRides entirely.
	SuspendedAt *time.Time
	// Code is the 6-character redemption credential. P1, bearer — never
	// logged, never in an error message. Empty unless Status is pending.
	Code             string
	Status           string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	AcceptedAt       *time.Time
	AcceptedByUserID string
	RevokedAt        *time.Time
}

// ShareGrant is the slim projection the redeem path returns: what the redeemer
// was granted, without any owner-facing field (no label, no code).
//
// Carries the FLAGS the redemption wrote, not the preset that produced them
// (MYR-369). The caller's job is to report what the grant now conveys, and the
// preset is one indirection away from that — a projection that returned it would
// re-emit the retired live_history for a legacy invite.
type ShareGrant struct {
	VehicleID   string
	OwnerUserID string
	AllowRides  bool
}

// PatchShareInviteInput is one owner edit of one ACCEPTED grant (MYR-369,
// PATCH /api/invites/{inviteId}).
//
// Both fields are POINTERS because absent and false are different requests: a
// nil AllowRides means "leave the ride capability exactly as it is", where a
// non-nil false means "take it away". Collapsing the two onto a bare bool would
// make every partial patch silently clear the field it did not mention — the
// classic partial-update bug, and here it would be an access-control one in both
// directions.
type PatchShareInviteInput struct {
	InviteID    string
	OwnerUserID string
	AllowRides  *bool
	Suspended   *bool
}

// HasChange reports whether the input asks for anything. An empty patch is
// rejected rather than treated as a successful no-op, so a client bug cannot
// look like an applied edit.
func (in PatchShareInviteInput) HasChange() bool {
	return in.AllowRides != nil || in.Suspended != nil
}

// CreateShareInviteInput is the create-time request. VehicleIDs is the full
// set the one minted code should grant — a single-vehicle invite is a
// one-element slice. Every id must be owned by OwnerUserID; the repository
// verifies that itself rather than trusting the caller.
type CreateShareInviteInput struct {
	OwnerUserID string
	// PathVehicleID is the vehicle whose row is returned to the caller. It
	// MUST appear in VehicleIDs; the handler rejects the request otherwise.
	PathVehicleID string
	VehicleIDs    []string
	// Label is P1 — never logged.
	Label      string
	Permission string
}

// ValidSharePermission reports whether s is one of the three presets. Still
// accepts the retired live_history — rejecting it would break invite creation
// for every un-updated client — which is why NormalizeSharePermission exists
// and why the write path is required to run through it.
func ValidSharePermission(s string) bool {
	switch s {
	case SharePermissionLive, SharePermissionLiveHistory, SharePermissionRides:
		return true
	default:
		return false
	}
}

// NormalizeSharePermission collapses the retired live_history preset onto live
// (MYR-369) so the value is never written again. Everything else passes through
// unchanged, including an unrecognized value: validation is
// ValidSharePermission's job, and quietly repairing garbage here would hide it.
func NormalizeSharePermission(s string) string {
	if s == SharePermissionLiveHistory {
		return SharePermissionLive
	}
	return s
}

// grantAllowsRides maps an invite-time preset onto the accepted grant's ride
// capability — the store-side half of the auth.GrantForPreset mapping, kept here
// so the redeem SQL can compute it without internal/store importing
// internal/auth (the dependency rule runs the other way).
//
// Fail-closed default: anything other than the rides preset — including the
// retired live_history and anything unrecognized — produces false.
func grantAllowsRides(permission string) bool {
	return permission == SharePermissionRides
}
