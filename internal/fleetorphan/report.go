package fleetorphan

import (
	"context"
	"errors"
	"time"
)

// ── SENTINELS ───────────────────────────────────────────────────────────────
//
// Both are returned by the cmd-side adapters, never produced in here, and both
// exist so this package can label an outcome without importing internal/store
// or internal/telemetry and without matching on error strings.
var (
	// ErrNoToken means the owner has no usable Tesla token: no "Account" row
	// (the deleted-account case) or a row whose ciphertext is absent. It is the
	// difference between `unreachable_no_token` and `failed`, and that
	// distinction is the whole point of the report — see the package doc.
	ErrNoToken = errors.New("fleetorphan: no tesla token for owner")

	// ErrNoConfig means Tesla says this VIN has no fleet-telemetry config —
	// including a 404, which the adapter maps here rather than surfacing as a
	// failure. Nothing to delete, nothing being billed.
	ErrNoConfig = errors.New("fleetorphan: no telemetry config at tesla")
)

// ── CANDIDATE SOURCES ───────────────────────────────────────────────────────

const (
	// SourceTombstone: a go_removed_vehicles row whose VIN no local vehicle
	// claims. We removed the car; whether Tesla still has a config is unknown
	// until we ask, and we can only ask if the owner is still linked.
	SourceTombstone = "tombstone"
	// SourceFleetList: Tesla's own answer to GET /api/1/vehicles for a live
	// owner named a VIN we hold no "Vehicle" row for. This is the reachable
	// set — we have the token by construction.
	SourceFleetList = "fleet_list"
)

// ── OUTCOME LABELS ──────────────────────────────────────────────────────────

const (
	// OutcomeWouldDelete: a config exists and -apply would remove it.
	OutcomeWouldDelete = "would_delete"
	// OutcomeDeleted: the DELETE returned success.
	OutcomeDeleted = "deleted"
	// OutcomeNoConfig: Tesla reports nothing configured (or 404s). Clean.
	OutcomeNoConfig = "no_config"
	// OutcomeUnreachableNoToken: no token exists for this VIN's owner, so no
	// call can be authenticated. The config — if any — keeps streaming and
	// billing until its 350-day exp lapses. This tool cannot fix it.
	OutcomeUnreachableNoToken = "unreachable_no_token"
	// OutcomeFailed: Tesla or the token path errored. Recorded, never fatal.
	OutcomeFailed = "failed"
)

// ── CONSUMER-SITE TYPES ─────────────────────────────────────────────────────
//
// Declared here rather than reused from internal/store or internal/telemetry so
// this package imports neither and every collaborator is trivially fakeable.
// cmd/sweep-orphan-fleet-configs owns the translation.

// TombstonedOrphan is one candidate from source A.
type TombstonedOrphan struct {
	UserID         string
	TeslaVehicleID string
	VIN            string
	RemovedAt      time.Time
}

// FleetVehicle is one entry of Tesla's answer to GET /api/1/vehicles.
type FleetVehicle struct {
	VIN string
	// Owner is Tesla's access_type == "OWNER". A shared-driver entry names
	// somebody else's car: we never provisioned it and must never delete its
	// config, so non-owner rows are dropped during collection.
	Owner bool
}

// ConfigStatus is the part of Tesla's fleet_telemetry_config GET this sweep
// reads.
type ConfigStatus struct {
	// Configured is true when Tesla holds a config for the VIN.
	Configured bool
	// Exp is the config's expiry as a Unix second, when Tesla echoes one. Nil
	// means unknown — Tesla documents `synced` but not that it returns `exp`.
	// Carried purely so the report can say how long an unreachable config has
	// left to bill.
	Exp *int64
}

// ── COLLABORATORS ───────────────────────────────────────────────────────────

// Store is the persistence surface. Every method is a read except the last.
type Store interface {
	ListTombstonedOrphanVINs(ctx context.Context, limit int) ([]TombstonedOrphan, error)
	ListTeslaLinkedUserIDs(ctx context.Context) ([]string, error)
	ListVehicleVINsByUser(ctx context.Context, userID string) ([]string, error)
	ListOrphanFleetConfigAttempts(ctx context.Context) ([]string, error)
	DeleteFleetConfigAttempts(ctx context.Context, vehicleIDs []string) (int64, error)
	// CountOrphanedTombstones / PurgeOrphanedTombstones cover the MYR-596
	// legacy backlog: go_removed_vehicles rows whose user_id resolves to no
	// identity at all, left behind by accounts deleted before step 8e existed.
	// Opt-in — see Config.PurgeOrphanTombstones.
	CountOrphanedTombstones(ctx context.Context) (int64, error)
	PurgeOrphanedTombstones(ctx context.Context) (int64, error)
}

// FleetLister enumerates a Tesla account's vehicles.
type FleetLister interface {
	ListVehicles(ctx context.Context, token string) ([]FleetVehicle, error)
}

// ConfigReader asks Tesla whether a VIN has a telemetry config. It returns
// ErrNoConfig for an absent one.
type ConfigReader interface {
	GetTelemetryConfig(ctx context.Context, token, vin string) (ConfigStatus, error)
}

// ConfigDeleter removes a VIN's telemetry config at Tesla. Called ONLY under
// -apply; a dry run never holds a non-nil deleter path.
type ConfigDeleter interface {
	DeleteTelemetryConfig(ctx context.Context, token, vin string) error
}

// TokenSource resolves an owner's Tesla access token, refreshing if it can. It
// returns ErrNoToken when there is nothing on file.
type TokenSource interface {
	AccessToken(ctx context.Context, userID string) (string, error)
}

// ── REPORT ──────────────────────────────────────────────────────────────────

// VINOutcome is one line of the per-VIN report.
type VINOutcome struct {
	// VIN is the FULL vin. The JSON report is an operator artifact run against
	// production and a redacted tail cannot be acted on; the slog lines this
	// package emits carry the redacted form instead.
	VIN string `json:"vin"`
	// UserID is the owner handle the token was looked up under.
	UserID string `json:"userId"`
	// Sources lists every candidate source that produced this VIN, in
	// collection order (tombstone before fleet_list).
	Sources []string `json:"sources"`
	// Outcome is one of the five labels above.
	Outcome string `json:"outcome"`
	// Detail is a human-readable reason, present on failed / unreachable. It
	// never contains token material, and any VIN embedded in a Tesla error
	// body is redacted to its last four (the line's own VIN field is the one
	// authoritative copy).
	Detail string `json:"detail,omitempty"`
	// ConfigExp echoes ConfigStatus.Exp when Tesla supplied one.
	ConfigExp *int64 `json:"configExp,omitempty"`
}

// AttemptsReport covers source C — the DB-only go_fleet_config_attempts
// garbage, which needs no Tesla call and so is the one part of this sweep that
// always works.
type AttemptsReport struct {
	// OrphanRows are the vehicle_ids found with no surviving "Vehicle" row.
	OrphanRows []string `json:"orphanRows"`
	// Deleted is how many were removed. Always 0 on a dry run.
	Deleted int64 `json:"deleted"`
}

// TombstonePurgeReport covers the MYR-596 legacy backlog: go_removed_vehicles
// rows belonging to accounts that were deleted before step 8e of the deletion
// sequence started taking them. Opt-in, and reported even when it did nothing
// so the report's shape does not change between runs.
type TombstonePurgeReport struct {
	// Requested is true when -purge-orphan-tombstones was passed. When false
	// the two numbers below are zero and mean nothing was looked at, which is
	// NOT the same as "there were none".
	Requested bool `json:"requested"`
	// Orphaned is how many rows matched the orphan predicate — the size of the
	// backlog, counted whether or not it was then deleted.
	Orphaned int64 `json:"orphaned"`
	// Deleted is how many were removed. Always 0 on a dry run.
	Deleted int64 `json:"deleted"`
}

// Report is the whole run.
type Report struct {
	// DryRun is true when nothing was written and no DELETE was issued.
	DryRun bool `json:"dryRun"`
	// Counts maps each outcome label to its VIN count. Present even when zero
	// so a diff between runs is stable.
	Counts map[string]int `json:"counts"`
	// VINs is the per-VIN report, VIN-sorted for a stable diff.
	VINs []VINOutcome `json:"vins"`
	// Attempts is the source-C result.
	Attempts AttemptsReport `json:"attempts"`
	// Tombstones is the MYR-596 legacy-backlog purge, which runs LAST so the
	// VINs above are reported before the rows naming them are removed.
	Tombstones TombstonePurgeReport `json:"tombstones"`
	// Errors are run-scoped failures that are not attributable to one VIN —
	// a candidate enumeration that failed, an owner whose fleet list errored.
	// VIN-scrubbed and length-capped like Detail. Their presence means the
	// sweep was INCOMPLETE, not that it was wrong.
	Errors []string `json:"errors,omitempty"`
}

// newReport seeds every label so the counts block has a fixed shape.
func newReport(dryRun bool) Report {
	return Report{
		DryRun: dryRun,
		Counts: map[string]int{
			OutcomeWouldDelete:        0,
			OutcomeDeleted:            0,
			OutcomeNoConfig:           0,
			OutcomeUnreachableNoToken: 0,
			OutcomeFailed:             0,
		},
	}
}
