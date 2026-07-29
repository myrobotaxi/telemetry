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

// Share permission tiers, in increasing-privilege order. STRICTLY CUMULATIVE:
// each tier grants everything below it plus its own increment, so consumers
// compare with a >= over this order rather than treating them as independent
// capabilities (vehicle-sharing.schema.json SharePermission).
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
	Label      string
	Permission string
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
type ShareGrant struct {
	VehicleID   string
	OwnerUserID string
	Permission  string
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

// ValidSharePermission reports whether s is one of the three tiers.
func ValidSharePermission(s string) bool {
	switch s {
	case SharePermissionLive, SharePermissionLiveHistory, SharePermissionRides:
		return true
	default:
		return false
	}
}
