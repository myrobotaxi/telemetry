package store

// MYR-593: the reads behind cmd/sweep-orphan-fleet-configs — a one-off operator
// sweep for Tesla fleet-telemetry configs whose local "Vehicle" row is gone.
//
// Three candidate sources, three shapes of garbage:
//
//   A. go_removed_vehicles (migration 0006) rows carrying a VIN that no
//      "Vehicle" row claims any more. These are cars we tore down locally; the
//      tombstone's user_id is the ONLY handle left on a token.
//   B. per-owner VIN sets, so the caller can diff a live owner's Tesla-side
//      fleet (Fleet API GET /api/1/vehicles) against what we actually hold.
//   C. go_fleet_config_attempts (migration 0031) rows keyed by a "Vehicle"."id"
//      that no longer exists. Migration 0031's header claims these are "swept
//      by the same DELETE path when the vehicle is torn down"; that became true
//      only with MYR-593's teardown change, so the backlog is still here.
//
// EVERY STATEMENT AGAINST "Vehicle" / "Account" IS A READ. The only write is
// the DELETE against go_fleet_config_attempts, a Go-owned table. That keeps the
// file inside data-lifecycle.md §1.4 without a carve-out (CG-DL-9 constrains
// Prisma-table WRITES).
//
// No metrics collaborator: this repo is constructed by a one-off binary that
// has no Prometheus registry and exits in seconds, so a metrics interface would
// be ceremony with no reader. It mirrors RemovedVehicleRegistry, not
// VehicleRepo.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TombstonedOrphanVIN is one removed-vehicle tombstone whose VIN no longer
// belongs to any local "Vehicle" row — source A above.
type TombstonedOrphanVIN struct {
	// UserID is the owner who removed the car. It is the only handle on a
	// Tesla token for this VIN; if that account has since been deleted, the
	// token is gone and the config is unreachable.
	UserID string
	// TeslaVehicleID is the tombstone's other key half. Carried for operator
	// correlation only — the fleet-telemetry config is addressed by VIN.
	TeslaVehicleID string
	// VIN is non-empty by construction (the query filters NULL and '').
	VIN string
	// RemovedAt is when the teardown wrote the tombstone.
	RemovedAt time.Time
}

// queryTombstonedOrphanVINs finds tombstones whose VIN is unclaimed locally.
//
// The NOT EXISTS is deliberately GLOBAL rather than per-owner: a VIN that has
// been re-added under ANY account has a live "Vehicle" row and a live config we
// must not touch. "Vehicle"."vin" is UNIQUE, so at most one row can claim it and
// the check is a single index probe.
//
// Ordered oldest-first: the longest-orphaned config is the one that has been
// billing the longest, so a limited run clears the most expensive backlog first.
const queryTombstonedOrphanVINs = `
SELECT rv.user_id, rv.tesla_vehicle_id, rv.vin, rv.removed_at
FROM go_removed_vehicles rv
WHERE NULLIF(rv.vin, '') IS NOT NULL
  AND NOT EXISTS (
        SELECT 1 FROM "Vehicle" v WHERE v."vin" = rv.vin
      )
ORDER BY rv.removed_at ASC
LIMIT $1`

// queryTeslaLinkedUserIDs lists every user who still has a Tesla OAuth grant on
// file. DISTINCT because "Account" is keyed by (provider, providerAccountId),
// not by user, so a relink history can leave more than one tesla row per user.
const queryTeslaLinkedUserIDs = `
SELECT DISTINCT "userId"
FROM "Account"
WHERE "provider" = 'tesla'
ORDER BY "userId"`

// queryVehicleVINsByUser lists the VINs this user actually holds locally — the
// right-hand side of the source-B diff.
const queryVehicleVINsByUser = `
SELECT v."vin"
FROM "Vehicle" v
WHERE v."userId" = $1
  AND NULLIF(v."vin", '') IS NOT NULL`

// queryOrphanFleetConfigAttempts finds schedule rows for vehicles that no
// longer exist. Unbounded on purpose: the table is keyed by vehicle and is
// self-draining, so the orphan set is small, and a LIMIT here would make
// "-apply cleared everything" untrue in a way an operator could not see.
const queryOrphanFleetConfigAttempts = `
SELECT fa.vehicle_id
FROM go_fleet_config_attempts fa
WHERE NOT EXISTS (
        SELECT 1 FROM "Vehicle" v WHERE v."id" = fa.vehicle_id
      )
ORDER BY fa.vehicle_id`

// queryDeleteOrphanFleetConfigAttempts removes the rows found above.
//
// IT RE-CHECKS NOT EXISTS. The id list comes from a read taken moments earlier,
// and re-proving the predicate inside the DELETE means this statement can only
// ever remove garbage: a caller that passed the wrong ids, or a vehicle
// re-provisioned between the read and the write, loses nothing. A live car's
// reconcile schedule is not something an operator tool should be able to drop
// by accident.
const queryDeleteOrphanFleetConfigAttempts = `
DELETE FROM go_fleet_config_attempts fa
WHERE fa.vehicle_id = ANY($1)
  AND NOT EXISTS (
        SELECT 1 FROM "Vehicle" v WHERE v."id" = fa.vehicle_id
      )`

// FleetConfigOrphanRepo answers the three orphan-candidate reads and owns the
// one write (the go_fleet_config_attempts cleanup).
type FleetConfigOrphanRepo struct {
	pool *pgxpool.Pool
}

// NewFleetConfigOrphanRepo builds the repo over the given pool.
func NewFleetConfigOrphanRepo(pool *pgxpool.Pool) *FleetConfigOrphanRepo {
	return &FleetConfigOrphanRepo{pool: pool}
}

// ListTombstonedOrphanVINs returns up to limit tombstones whose VIN no local
// vehicle claims, oldest removal first.
//
// A non-positive limit returns no rows and no error, matching
// ListFleetConfigCandidates: refusing an unbounded scan of a Prisma-correlated
// query is the safer reading of a zero.
func (r *FleetConfigOrphanRepo) ListTombstonedOrphanVINs(
	ctx context.Context, limit int,
) ([]TombstonedOrphanVIN, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, queryTombstonedOrphanVINs, limit)
	if err != nil {
		return nil, fmt.Errorf("store.ListTombstonedOrphanVINs: %w", err)
	}
	defer rows.Close()

	out := make([]TombstonedOrphanVIN, 0, limit)
	for rows.Next() {
		var t TombstonedOrphanVIN
		if err := rows.Scan(&t.UserID, &t.TeslaVehicleID, &t.VIN, &t.RemovedAt); err != nil {
			return nil, fmt.Errorf("store.ListTombstonedOrphanVINs: scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListTombstonedOrphanVINs: rows: %w", err)
	}
	return out, nil
}

// ListTeslaLinkedUserIDs returns every user id with a Tesla "Account" row —
// exactly the users whose Tesla-side fleet is still reachable with a token we
// hold. Users whose account was deleted are, by construction, absent.
func (r *FleetConfigOrphanRepo) ListTeslaLinkedUserIDs(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, queryTeslaLinkedUserIDs)
	if err != nil {
		return nil, fmt.Errorf("store.ListTeslaLinkedUserIDs: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store.ListTeslaLinkedUserIDs: scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListTeslaLinkedUserIDs: rows: %w", err)
	}
	return out, nil
}

// ListVehicleVINsByUser returns the non-empty VINs this user holds locally.
func (r *FleetConfigOrphanRepo) ListVehicleVINsByUser(
	ctx context.Context, userID string,
) ([]string, error) {
	rows, err := r.pool.Query(ctx, queryVehicleVINsByUser, userID)
	if err != nil {
		return nil, fmt.Errorf("store.ListVehicleVINsByUser(user=%s): %w", userID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var vin string
		if err := rows.Scan(&vin); err != nil {
			return nil, fmt.Errorf("store.ListVehicleVINsByUser(user=%s): scan: %w", userID, err)
		}
		out = append(out, vin)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListVehicleVINsByUser(user=%s): rows: %w", userID, err)
	}
	return out, nil
}

// ListOrphanFleetConfigAttempts returns every go_fleet_config_attempts
// vehicle_id with no surviving "Vehicle" row — source C, the DB-only garbage
// that needs no Tesla call to clear.
func (r *FleetConfigOrphanRepo) ListOrphanFleetConfigAttempts(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, queryOrphanFleetConfigAttempts)
	if err != nil {
		return nil, fmt.Errorf("store.ListOrphanFleetConfigAttempts: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store.ListOrphanFleetConfigAttempts: scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListOrphanFleetConfigAttempts: rows: %w", err)
	}
	return out, nil
}

// DeleteFleetConfigAttempts removes the named schedule rows, re-proving they
// are orphaned inside the DELETE itself. Returns the number of rows removed,
// which may be lower than len(vehicleIDs) if a vehicle reappeared. An empty
// slice is a no-op success.
func (r *FleetConfigOrphanRepo) DeleteFleetConfigAttempts(
	ctx context.Context, vehicleIDs []string,
) (int64, error) {
	if len(vehicleIDs) == 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, queryDeleteOrphanFleetConfigAttempts, vehicleIDs)
	if err != nil {
		return 0, fmt.Errorf("store.DeleteFleetConfigAttempts: %w", err)
	}
	return tag.RowsAffected(), nil
}
