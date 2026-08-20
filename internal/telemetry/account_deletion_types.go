package telemetry

import "context"

// The consumer-site seams AccountDeletionHandler needs (MYR-355). Each is
// deliberately the NARROWEST view of a type that already exists, because the
// endpoint's whole design is to COMPOSE the machinery the per-vehicle teardown
// and the ride lifecycle already provide rather than to reimplement deletion.

// AccountOwnedVehicleLister lists the vehicle ids a user owns. Backed by
// store.VehicleRepo.ListByUser. Used by TeslaLinkRevoker's last-vehicle
// pre-check; the deletion sequence itself needs VINs too and so reads through
// AccountOwnedVehicleReader below.
type AccountOwnedVehicleLister interface {
	ListOwnedVehicleIDs(ctx context.Context, userID string) ([]string, error)
}

// OwnedVehicle is the two facts the deletion sequence needs about one owned
// car: which row to tear down, and which VIN to stop streaming at Tesla.
//
// The VIN is here rather than resolved per-car later because the two uses must
// come from ONE read. Listing the fleet twice — once to delete configs, once to
// delete rows — would let a car added or removed between the two reads be
// billed-but-not-torn-down, or torn down with its config left running, which is
// the exact failure MYR-593 is about.
type OwnedVehicle struct {
	// ID is the Prisma "Vehicle"."id" cuid the teardown transaction is keyed on.
	ID string
	// VIN may be empty: the Prisma column is nullable, and a car linked but
	// never synced has no VIN yet. An empty VIN has no Tesla-side config to
	// delete and is skipped for that step alone — the row teardown still runs.
	VIN string
}

// AccountOwnedVehicleReader lists the cars an account owns, id AND VIN, so the
// deletion sequence can both stop them streaming at Tesla and tear down their
// rows from a single view of the fleet. Backed by store.VehicleRepo.ListByUser
// through the same adapter that serves AccountOwnedVehicleLister.
type AccountOwnedVehicleReader interface {
	ListOwnedVehicles(ctx context.Context, userID string) ([]OwnedVehicle, error)
}

// AccountStreamConfigDeleter stops ONE owned car streaming at Tesla, keyed by
// the owner whose token authenticates the call. Satisfied by
// *StreamConfigTeardown — the same value the per-vehicle teardown endpoint
// holds, which is the point: MYR-593 was two implementations of "remove a
// vehicle", one of which forgot Tesla.
//
// The bool is "did Tesla accept it". There is no error in the signature, for
// the same reason AccountTeslaLinkRevoker has none: a value that cannot be
// returned up the stack cannot accidentally fail somebody's account deletion.
// Nil is legal and means the deployment has no tesla-http-proxy configured, so
// there is no call to make.
type AccountStreamConfigDeleter interface {
	DeleteStreamConfig(ctx context.Context, userID, vin string) bool
}

// AccountRideCanceller lists the user's OPEN rides as RIDER and transitions
// them through the SAME guarded status write the rider-facing cancel endpoint
// uses (store.RideRequestRepo.ListOpenByRider + UpdateStatusFrom). Going
// through the guard rather than a bulk UPDATE is the point: the transition
// stays legal, the race with an owner accept/decline still resolves in the
// database, and the winning write is the one that publishes.
type AccountRideCanceller interface {
	ListOpenRidesByRider(ctx context.Context, riderID string) ([]OpenRideRef, error)
	UpdateStatusFrom(ctx context.Context, id string, from []string, to string) (RideRequestData, error)
}

// OpenRideRef is the two facts the sweep needs about an open ride: which one,
// and whether the cancel transition is legal from where it currently is.
// Deliberately not a whole RideRequestData — the list path has no use for the
// P1 pickup/dropoff coordinates a full record would decrypt, and the guarded
// UPDATE's own return already carries everything the published event needs.
type OpenRideRef struct {
	ID     string
	Status string
}

// AccountDataDeleter is the store-side half of the deletion — the steps that
// are pure SQL. Backed by store.AccountDeleter. Every method is idempotent:
// a re-run affects zero rows and reports zero.
type AccountDataDeleter interface {
	ResolveDeletionScope(ctx context.Context, callerID string) (AccountDeletionScope, error)
	CountUserDrives(ctx context.Context, userID string) (int, error)
	RevokeSharesReceived(ctx context.Context, userID string) (int, error)
	ScrubSharesReceivedLabel(ctx context.Context, userID string) (int, error)
	DeletePushDevices(ctx context.Context, userID string) (int, error)
	DeleteSavedPlaces(ctx context.Context, userID string) (int, error)
	DeleteProfileNameConfirmation(ctx context.Context, userID string) (int, error)
	// DeleteUserActivity drops the account's last-seen row (MYR-592,
	// data-lifecycle.md §3.1 step 8c). P1 behavioural data; see the store
	// query for why it is a delete and not a tombstone.
	DeleteUserActivity(ctx context.Context, userID string) (int, error)
	// DeleteTeslaTokenKeepalive drops the account's keepalive bookkeeping
	// (MYR-594, data-lifecycle.md §3.1 step 8d). P0 hygiene rather than an
	// erasure obligation; see the store query for why it goes regardless.
	DeleteTeslaTokenKeepalive(ctx context.Context, userID string) (int, error)
	// DeleteRemovedVehicleTombstones drops the account's removed-vehicle
	// tombstones (MYR-596, data-lifecycle.md §3.1 step 8e). It is the ONE
	// position-constrained member of the 8-family: the per-vehicle teardown
	// writes a tombstone per car, so this must run after it.
	DeleteRemovedVehicleTombstones(ctx context.Context, userID string) (int, error)
	DeleteRideMemberships(ctx context.Context, userID string) (int, error)
	RevokeRefreshTokens(ctx context.Context, userID string) (int, error)
	DeleteIdentity(ctx context.Context, scope AccountDeletionScope, counts AccountDeletionCounts) (AccountIdentityOutcome, error)
}

// AccountDeletionScope mirrors store.DeletionScope: the set of user ids that
// constitute one human (MYR-452).
//
// The caller's JWT subject is not reliably the id their account is filed under.
// A Tesla link that converges two identities re-points the Apple binding onto a
// canonical id and abandons the caller's original one, and nothing re-issues the
// caller's tokens — so the subject arriving here can name an id that owns
// nothing while the real account, binding included, sits under another. Running
// the teardown over the closure instead of the bare subject is what stops a
// "deleted" account from being recognised and signed back into.
type AccountDeletionScope struct {
	// CallerID is the JWT subject as presented.
	CallerID string
	// CanonicalID is the id that owns the identity rows; the audit row is
	// filed against it.
	CanonicalID string
	// IDs is every id in the closure, CanonicalID first. Usually just one.
	IDs []string
}

// Converged reports whether the caller authenticated under an id other than the
// one that owns the account, or trails abandoned aliases. P0-safe to log.
func (s AccountDeletionScope) Converged() bool {
	return s.CanonicalID != s.CallerID || len(s.IDs) > 1
}

// AccountDeletionCounts is the P0-only audit tally, mirroring
// store.AccountDeletionCounts field for field (the cmd adapter converts).
type AccountDeletionCounts struct {
	VehicleCount        int
	DriveCount          int
	RidesCancelled      int
	SharesRevoked       int
	ShareLabelsScrubbed int
	PushDevicesDeleted  int
	SavedPlacesDeleted  int
	// ProfileNameConfirmationsDeleted counts the display-name confirmation rows
	// removed (MYR-583) — 0 or 1, since the table is keyed by user id.
	ProfileNameConfirmationsDeleted int
	// UserActivityRowsDeleted counts the last-seen rows removed (MYR-592).
	// 0 or 1 per identity in the deletion scope; 0 for an account that never
	// authenticated after migration 0043 shipped.
	UserActivityRowsDeleted int
	// TeslaTokenKeepaliveRowsDeleted counts the keepalive bookkeeping rows
	// removed (MYR-594). 0 for every account the keepalive arm never tried,
	// which is almost all of them.
	TeslaTokenKeepaliveRowsDeleted int
	// RemovedVehicleTombstonesDeleted counts the removed-vehicle tombstones
	// removed (MYR-596) — one per car this person ever removed, 0 for the
	// large majority who never removed one.
	RemovedVehicleTombstonesDeleted int
	// RideMembershipsDeleted counts the GROUP-RIDE memberships the account
	// held (MYR-540) — the rides they JOINED, as against RidesCancelled, the
	// rides they BOOKED.
	RideMembershipsDeleted int
	RefreshTokensRevoked   int
}

// AccountIdentityOutcome mirrors store.AccountIdentityResult.
type AccountIdentityOutcome struct {
	Deleted       bool
	AlreadyGone   bool
	HadPrismaUser bool
	AuditLogID    string
}

// AccountTeslaLinkRevoker actively revokes the caller's Tesla OAuth grant at
// Tesla, BEFORE any later step deletes the stored tokens (MYR-366). Backed by
// telemetry.TeslaLinkRevoker. The bool is "did Tesla accept it" — there is no
// error, because no answer from Tesla may block a deletion. Nil is legal: the
// deployment has no Tesla OAuth client configured, and the tokens are simply
// deleted without a revoke call, exactly as before MYR-366.
type AccountTeslaLinkRevoker interface {
	RevokeTeslaLink(ctx context.Context, userID string) bool
}

// AccountSessionInvalidator drops the caches that would otherwise keep a
// deleted user's still-unexpired access token working. Backed by
// auth.JWTAuthenticator. Nil is legal — the caches expire on their own, this
// just closes the window immediately.
type AccountSessionInvalidator interface {
	InvalidateUser(userID string)
	InvalidateVehicles(userID string)
}

// AccountDeletionDeps bundles the handler's collaborators. It exists so the
// handler struct stays under the 7-field decomposition rule and so the
// composition root names every seam explicitly at the call site.
type AccountDeletionDeps struct {
	// Vehicles reads the caller's owned cars — id and VIN — once, for both the
	// Tesla-side config delete and the row teardown.
	Vehicles AccountOwnedVehicleReader
	// StreamConfigs stops each owned car streaming at Tesla (MYR-593), before
	// TeslaLink revokes the grant those calls authenticate with. Nil skips the
	// step, which is the state of any deployment with no proxy configured.
	StreamConfigs AccountStreamConfigDeleter
	// TeslaLink revokes the caller's Tesla OAuth grant at Tesla before any
	// step deletes the stored tokens (MYR-366). Nil skips the revoke.
	TeslaLink AccountTeslaLinkRevoker
	// Teardown is the MYR-258 per-vehicle teardown, reused verbatim (step 1).
	Teardown VehicleTeardownWriter
	// Rides cancels the caller's open rides as RIDER (step 3).
	Rides AccountRideCanceller
	// Events publishes the ride_status_changed lifecycle event for each
	// cancellation, which is what reaches the affected owners (WS + push).
	// Nil makes the publish a no-op.
	Events RideEventPublisher
	// Data performs the SQL-only steps and the final identity transaction.
	Data AccountDataDeleter
	// Sessions drops the auth caches after the identity rows go. Nil is legal.
	Sessions AccountSessionInvalidator
}
