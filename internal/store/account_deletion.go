package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AccountDeleter is the store-side half of user-initiated account deletion
// (MYR-355, FR-10.1/FR-10.2, docs/contracts/rest-api.md §7.6). It owns the
// account-scoped steps that are pure SQL; the ORDER of the whole deletion —
// and the two steps that must run through existing application machinery
// (per-vehicle teardown, guarded ride cancellation) — lives in
// telemetry.AccountDeletionHandler, because those steps publish events and
// only the handler holds those seams.
//
// WHY THIS IS NOT ONE TRANSACTION. The per-vehicle teardown
// (store.OwnerTeardown) is already its own transaction, by design: it takes
// FOR UPDATE locks over the owner's vehicle set to make the last-vehicle
// decision race-safe, and it fires a `vehicle_deleted` NOTIFY whose consumers
// must not observe an uncommitted delete. Wrapping N teardowns plus a ride
// cancellation that publishes push notifications inside one outer transaction
// would either deadlock against those locks or emit notifications for work
// that a later rollback undid. So the deletion is a SEQUENCE of independently
// atomic steps, and the property we buy instead of one-shot atomicity is that
// **every step is idempotent and the sequence is RE-RUNNABLE**: a mid-failure
// leaves the account partially deleted, the endpoint answers 500, and calling
// it again resumes from wherever it stopped. The steps are ordered so that
// resuming is always possible — the identity rows the caller authenticates
// with are deleted LAST, so a failure never locks the user out of finishing
// their own deletion.
//
// Contract position (data-lifecycle.md §1.4 / §3): this extends the MYR-258
// owner-teardown carve-out from "one vehicle" to "the account", and moves
// ownership of the FR-10.1 deletion transaction from the Next.js app to the Go
// server, because the native iOS client (P9) never talks to the Next.js app at
// all. The `account_deleted` AuditLog row that §3.1 assigns to Prisma is now
// written here, in the same transaction as the identity delete (CG-DL-3).
type AccountDeleter struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewAccountDeleter builds the account-deletion writer over the given pool.
func NewAccountDeleter(pool *pgxpool.Pool, logger *slog.Logger) *AccountDeleter {
	if logger == nil {
		logger = slog.Default()
	}
	return &AccountDeleter{pool: pool, logger: logger}
}

// AccountDeletionCounts is the P0-only tally the handler accumulates across
// the sequence and hands to DeleteIdentity for the audit metadata. Every field
// is an opaque count or a bool — never PII, never a name, never a coordinate
// (CG-DL-5).
type AccountDeletionCounts struct {
	// VehicleCount is the number of owned vehicles torn down.
	VehicleCount int `json:"vehicleCount"`
	// DriveCount is the number of drives that went with those vehicles,
	// counted BEFORE the teardowns ran.
	DriveCount int `json:"driveCount"`
	// RidesCancelled is the number of open ride requests cancelled with the
	// user as RIDER.
	RidesCancelled int `json:"ridesCancelled"`
	// SharesRevoked is the number of accepted grants the user had REDEEMED
	// that were tombstoned.
	SharesRevoked int `json:"sharesRevoked"`
	// ShareLabelsScrubbed is the number of share rows whose owner-typed label
	// — the deleted person's NAME, written by somebody else — was erased
	// (MYR-447). A count, never the labels: they are P1 and the audit row is
	// P0-only (CG-DL-5). Counted separately from SharesRevoked because the two
	// steps deliberately cover different row sets: revocation skips rows that
	// were already revoked, the scrub does not.
	ShareLabelsScrubbed int `json:"shareLabelsScrubbed"`
	// PushDevicesDeleted is the number of APNs registrations removed.
	PushDevicesDeleted int `json:"pushDevicesDeleted"`
	// SavedPlacesDeleted is the number of saved Home/Work rows removed —
	// 0, 1 or 2. A COUNT, never the places themselves: the coordinates are P1
	// and the audit row is P0-only (CG-DL-5), so what is recorded is that two
	// rows went, never where they pointed.
	SavedPlacesDeleted int `json:"savedPlacesDeleted"`
	// ProfileNameConfirmationsDeleted is the number of display-name confirmation
	// rows removed — 0 or 1 (MYR-583), since the table's primary key is the user
	// id. A COUNT, and there is nothing else it could be: the row holds an
	// opaque cuid and a timestamp and never held the name (CG-DL-5 satisfied by
	// the table's shape, not just by this field). Recorded because the audit row
	// is how a deletion is shown to have reached every table that named the
	// person, and "did we take the consent record too" is a question the trail
	// must answer.
	ProfileNameConfirmationsDeleted int `json:"profileNameConfirmationsDeleted"`
	// RideMembershipsDeleted is the number of GROUP-RIDE memberships the user
	// held that were deleted (MYR-540) — the rides they JOINED, as opposed to
	// RidesCancelled, which counts the rides they BOOKED. A count, never a ride
	// id and never the other parties: the audit row is P0-only (CG-DL-5).
	RideMembershipsDeleted int `json:"rideMembershipsDeleted"`
	// RefreshTokensRevoked is the number of live refresh tokens revoked.
	RefreshTokensRevoked int `json:"refreshTokensRevoked"`
	// UserActivityRowsDeleted is the number of last-seen rows removed
	// (MYR-592, §3.1 step 8c) — 0 or 1, since go_user_activity is keyed by the
	// user id. A count of a P0-only row (opaque cuid + timestamp; the P1
	// last-seen SIGNAL never leaves the server). Recorded so the audit trail
	// answers "did the deletion reach the activity table" the same way it
	// answers it for every sibling go_ table.
	UserActivityRowsDeleted int `json:"userActivityRowsDeleted"`
	// TeslaTokenKeepaliveRowsDeleted is the number of keepalive bookkeeping
	// rows removed (MYR-594, §3.1 step 8d) — 0 or 1, keyed by user id. Every
	// column of that table is P0 by design (it records that a rotation was
	// ATTEMPTED, never the credential), so the count is as safe as its
	// siblings above.
	TeslaTokenKeepaliveRowsDeleted int `json:"teslaTokenKeepaliveRowsDeleted"`
	// RemovedVehicleTombstonesDeleted is the number of removed-vehicle
	// tombstones removed (MYR-596, §3.1 step 8e) — one per car this person ever
	// removed, usually 0. A COUNT and never the VINs: the row pairs an opaque
	// cuid with a VIN, and a VIN is P1 (data-classification.md §2.1), so the
	// only thing this field may carry across the CG-DL-5 boundary is how many
	// went. Recorded because the audit trail is how a deletion is shown to have
	// reached every table that named the person, and this is the last go_ table
	// that used to be exempt.
	RemovedVehicleTombstonesDeleted int `json:"removedVehicleTombstonesDeleted"`
	// HadPrismaUser records whether a sibling-schema "User" row existed —
	// the dual-source identity fact, and the one thing that distinguishes an
	// Apple-native account from a legacy web one in the audit trail.
	HadPrismaUser bool `json:"hadPrismaUser"`
}

// CountUserDrives counts the drives attached to the user's vehicles. Called
// once, before the teardowns, purely so the audit metadata can carry the
// number; a failure here is NOT fatal to the deletion (the handler logs and
// proceeds with zero) because a missing statistic must never block a user's
// right to erasure.
func (a *AccountDeleter) CountUserDrives(ctx context.Context, userID string) (int, error) {
	if strings.TrimSpace(userID) == "" {
		return 0, fmt.Errorf("store.CountUserDrives: empty user id")
	}
	var n int
	if err := a.pool.QueryRow(ctx, queryCountUserDrives, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountUserDrives(user=%s): %w", userID, err)
	}
	return n, nil
}

// RevokeSharesReceived tombstones every live grant the user redeemed — the
// shares they RECEIVED, as opposed to the shares on their own cars, which the
// per-vehicle teardown already revokes (MYR-184). Returns the number of rows
// revoked. Idempotent: a re-run matches nothing.
func (a *AccountDeleter) RevokeSharesReceived(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "RevokeSharesReceived", queryRevokeSharesReceived, userID)
}

// ScrubSharesReceivedLabel erases the owner-typed label from every share row
// the user had redeemed (MYR-447), so a third party's list does not keep the
// name of a person who deleted their account. Returns the number of rows
// scrubbed. Idempotent: a re-run matches nothing.
//
// Ordering note: this runs immediately after the revocation it completes, and
// its position is otherwise unconstrained — nothing later in the sequence reads
// the label. It is deliberately NOT folded into the revoke; see
// queryScrubSharesReceivedLabel for why the row sets differ.
func (a *AccountDeleter) ScrubSharesReceivedLabel(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "ScrubSharesReceivedLabel", queryScrubSharesReceivedLabel, userID)
}

// DeletePushDevices removes every APNs registration owned by the user, so no
// device keeps receiving ride notifications addressed to a deleted account.
// Returns the number of rows deleted. Idempotent.
func (a *AccountDeleter) DeletePushDevices(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "DeletePushDevices", queryDeletePushDevicesForUser, userID)
}

// DeleteSavedPlaces removes both of the user's saved-place slots (MYR-321), so
// the ciphertext of where they live does not outlive the account that saved it.
// Returns the number of rows deleted (0, 1 or 2). Idempotent.
//
// Ordering note: this runs BEFORE the identity delete like every other
// destructive step, and its position is otherwise unconstrained — the rows are
// keyed only by user_id, nothing else in the sequence reads them, and no
// teardown, cascade or event depends on them. It is slotted next to the push
// devices because both are "personal effects with no counterparty": rows that
// belong to this person alone and that nobody else has a claim on.
func (a *AccountDeleter) DeleteSavedPlaces(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "DeleteSavedPlaces", queryDeleteSavedPlacesForUser, userID)
}

// DeleteProfileNameConfirmation removes the person's display-name confirmation
// row (MYR-583), so the record that they approved a name does not outlive the
// account the name belonged to. Returns the number of rows deleted (0 or 1).
// Idempotent.
//
// Ordering note: it runs immediately after the saved places, among the personal
// effects, and its position there is unconstrained for the same reasons — the row
// is keyed only by user_id, nothing later in the sequence reads it, and no
// teardown, cascade or event depends on it. It belongs in that group rather than
// beside the identity delete because it is not identity: it names no rung and
// authenticates nothing.
//
// A REAL DELETE, not a tombstone, and the argument is shorter than the saved
// places' because the row is P0: what makes it go is not its sensitivity but the
// fact that it would otherwise be a standing assertion about a person who no
// longer exists, keyed by a cuid nothing resolves. It is also the ONLY delete this
// feature has — confirmation is monotonic everywhere else (see
// profile_name_confirmation.go).
func (a *AccountDeleter) DeleteProfileNameConfirmation(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "DeleteProfileNameConfirmation", queryDeleteProfileNameConfirmationForUser, userID)
}

// DeleteUserActivity removes the person's last-seen row (MYR-592), so the
// behavioural record of when they last used the product does not outlive the
// account. Returns the number of rows deleted (0 or 1). Idempotent.
//
// Ordering note: it sits with the personal effects, immediately after the
// display-name confirmation, and its position there is unconstrained for the
// same reasons — keyed only by user_id, read by nothing later in the sequence,
// and depended on by no teardown, cascade or event. Unlike the confirmation it
// is P1 rather than P0, which is what makes deleting it an erasure obligation
// rather than tidiness: see queryDeleteUserActivityForUser.
func (a *AccountDeleter) DeleteUserActivity(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "DeleteUserActivity", queryDeleteUserActivityForUser, userID)
}

// DeleteTeslaTokenKeepalive removes the person's keepalive bookkeeping
// (MYR-594), so no cooldown outlives the account it was recorded against.
// Returns the number of rows deleted (0 or 1). Idempotent.
//
// Sits immediately after the last-seen row and is unconstrained in the same
// way — keyed only by user_id, read by nothing later in the sequence. Unlike
// its neighbour it is P0 hygiene rather than a P1 erasure obligation; see
// queryDeleteTeslaTokenKeepaliveForUser for why it goes regardless.
func (a *AccountDeleter) DeleteTeslaTokenKeepalive(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "DeleteTeslaTokenKeepalive", queryDeleteTeslaTokenKeepaliveForUser, userID)
}

// DeleteRemovedVehicleTombstones removes the person's removed-vehicle
// tombstones (MYR-596), which protect a LIVE account's next Tesla sync and
// protect nothing at all once the account is gone. Returns the number of rows
// deleted (0..n, one per car this person ever removed). Idempotent.
//
// ORDERING IS NORMATIVE HERE, unlike every other member of the 8-family: it MUST
// run AFTER step 3. The per-vehicle teardown writes a tombstone for each car it
// removes, in the same transaction as the Vehicle delete (§1.4.1), so a purge
// placed before it would be undone car-for-car and the account would finish the
// sequence with a fresh, complete set of tombstones. See
// queryDeleteRemovedVehiclesForUser for the rest of the argument.
func (a *AccountDeleter) DeleteRemovedVehicleTombstones(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "DeleteRemovedVehicleTombstones", queryDeleteRemovedVehiclesForUser, userID)
}

// DeleteRideMemberships removes every group-ride membership the user holds
// (MYR-540), so a deleted account cannot keep appearing in a live ride's
// `members` array or in the access set that admits a WebSocket to that ride's
// vehicle. Returns the number of rows deleted. Idempotent.
//
// Ordering note: it runs immediately after the open-ride cancellation, because
// the two are the same fact from the two sides — that step ends the rides this
// person BOOKED, this one ends the rides they merely JOINED. It must run before
// the identity delete for the same reason every destructive step does, and
// nothing later in the sequence reads these rows.
func (a *AccountDeleter) DeleteRideMemberships(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "DeleteRideMemberships", queryDeleteRideMembershipsForUser, userID)
}

// RevokeRefreshTokens revokes every live refresh token in the user's name, so
// no stored session can mint a new access token after the identity rows go.
// Returns the number of rows revoked. Idempotent.
//
// Ordering note: this runs BEFORE the identity delete but AFTER every other
// destructive step, and it deliberately does NOT invalidate the caller's
// current ACCESS token — that token is what authenticates the re-run if the
// identity transaction then fails.
func (a *AccountDeleter) RevokeRefreshTokens(ctx context.Context, userID string) (int, error) {
	return a.execCount(ctx, "RevokeRefreshTokens", queryRevokeRefreshTokensForUser, userID)
}

// execCount runs a one-parameter mutating statement and returns the affected
// row count, wrapping the error with the operation name and the user id (P0).
func (a *AccountDeleter) execCount(ctx context.Context, op, query, userID string) (int, error) {
	if strings.TrimSpace(userID) == "" {
		return 0, fmt.Errorf("store.%s: empty user id", op)
	}
	tag, err := a.pool.Exec(ctx, query, userID)
	if err != nil {
		return 0, fmt.Errorf("store.%s(user=%s): %w", op, userID, err)
	}
	return int(tag.RowsAffected()), nil
}
