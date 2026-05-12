// Package routeblobbackfill encrypts the two pre-existing route-blob
// plaintext columns into their *Enc ciphertext shadows during the
// MYR-64 cross-repo route-blob encryption rollout (NFR-3.23):
//
//   - Vehicle.navRouteCoordinates → Vehicle.navRouteCoordinatesEnc
//     (Tesla's planned navigation polyline, navigation atomic group).
//   - Drive.routePoints → Drive.routePointsEnc (recorded breadcrumb
//     trail of completed drives).
//
// It is the dual-write companion to DriveRepo / VehicleRepo: those
// repos handle new writes; this package handles the legacy backlog.
//
// Idempotent. Re-running over a fully migrated table touches zero rows.
//
// The package is intentionally separate from internal/store so the
// running telemetry server doesn't pull in the backfill code path. The
// CLI in cmd/backfill-route-blobs/ is the canonical operator entry
// point; it can also be invoked from tests.
package routeblobbackfill

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store/routeblob"
)

// Column is a (table, plaintext column, ciphertext column) triple. The
// rollout's gauge labels rows by the plaintext column name so the same
// label naming is shared by every consumer.
type Column struct {
	Table     string
	Plaintext string
	Encrypted string
}

// Columns is the canonical iteration order. Vehicle first so the
// gauge always reports the navigation pair before the drive pair —
// stable ordering simplifies operator dashboards.
var Columns = []Column{
	{Table: "Vehicle", Plaintext: "navRouteCoordinates", Encrypted: "navRouteCoordinatesEnc"},
	{Table: "Drive", Plaintext: "routePoints", Encrypted: "routePointsEnc"},
}

// Result reports the outcome of a Run. Counts are tallied in-process
// and are independent of the post-run plaintext-remaining check.
type Result struct {
	// VehicleRowsScanned is how many Vehicle rows the SELECT returned.
	VehicleRowsScanned int
	// VehicleBlobsEncrypted counts non-empty Vehicle ciphertext writes.
	VehicleBlobsEncrypted int
	// VehicleRowsUpdated is the distinct count of UPDATE-touched rows.
	VehicleRowsUpdated int
	// DriveRowsScanned mirrors VehicleRowsScanned for the Drive table.
	DriveRowsScanned int
	// DriveBlobsEncrypted counts non-empty Drive ciphertext writes.
	DriveBlobsEncrypted int
	// DriveRowsUpdated mirrors VehicleRowsUpdated for Drive.
	DriveRowsUpdated int
	// EncryptErrors is the cross-table count of encrypt failures.
	EncryptErrors int
	// UpdateErrors is the cross-table count of UPDATE failures.
	UpdateErrors int
	// PlaintextRemaining is the post-run snapshot. Keys are
	// `<table>.<plaintext-col>` (e.g. "Vehicle.navRouteCoordinates")
	// so the metric labels and the JSON report agree.
	PlaintextRemaining map[string]int
}

// Errors reports whether any row failed mid-run. Used by the CLI's
// non-zero exit decision.
func (r Result) Errors() int { return r.EncryptErrors + r.UpdateErrors }

// pool is the subset of *pgxpool.Pool the backfill uses. Defining a
// narrow interface keeps tests light.
type pool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Backfiller runs the legacy → ciphertext migration for route-blob
// rows. Construct via New; the zero value is unusable.
type Backfiller struct {
	pool      pool
	encryptor cryptox.Encryptor
	logger    *slog.Logger
}

// New returns a Backfiller bound to the given pool + encryptor. The
// encryptor MUST be the same one wired into the running server.
//
// Panics on a nil Encryptor — the dual-write contract fails loud.
func New(p *pgxpool.Pool, enc cryptox.Encryptor, logger *slog.Logger) *Backfiller {
	if enc == nil {
		panic("routeblobbackfill.New: encryptor must not be nil")
	}
	return &Backfiller{pool: p, encryptor: enc, logger: logger}
}

// Run scans every Vehicle / Drive row that holds a non-empty plaintext
// blob without a ciphertext shadow, encrypts it, and updates the
// shadow column. Returns a Result regardless of per-row failures; the
// caller decides whether to exit non-zero based on Result.Errors().
func (b *Backfiller) Run(ctx context.Context) (Result, error) {
	res := Result{PlaintextRemaining: map[string]int{}}

	if err := b.runOneTable(ctx, &res, Columns[0],
		&res.VehicleRowsScanned, &res.VehicleBlobsEncrypted, &res.VehicleRowsUpdated); err != nil {
		return res, err
	}
	if err := b.runOneTable(ctx, &res, Columns[1],
		&res.DriveRowsScanned, &res.DriveBlobsEncrypted, &res.DriveRowsUpdated); err != nil {
		return res, err
	}

	remaining, rErr := CountPlaintextRemaining(ctx, b.pool)
	if rErr != nil {
		return res, fmt.Errorf("count plaintext remaining: %w", rErr)
	}
	res.PlaintextRemaining = remaining
	return res, nil
}

// runOneTable encrypts every row in the given table whose plaintext
// blob is non-empty and whose ciphertext shadow is NULL.
func (b *Backfiller) runOneTable(
	ctx context.Context,
	res *Result,
	col Column,
	scannedCounter, encryptedCounter, updatedCounter *int,
) error {
	sql := fmt.Sprintf(
		`SELECT "id", %q FROM %q WHERE %q IS NOT NULL AND %q IS NULL`,
		col.Plaintext, col.Table, col.Plaintext, col.Encrypted,
	)
	rows, err := b.pool.Query(ctx, sql)
	if err != nil {
		return fmt.Errorf("routeblobbackfill: select %s: %w", col.Table, err)
	}
	defer rows.Close()

	for rows.Next() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("routeblobbackfill: cancelled: %w", ctxErr)
		}
		var id string
		var raw []byte
		if scanErr := rows.Scan(&id, &raw); scanErr != nil {
			return fmt.Errorf("routeblobbackfill: scan %s row: %w", col.Table, scanErr)
		}
		*scannedCounter++
		b.encryptOneRow(ctx, res, col, id, raw, encryptedCounter, updatedCounter)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("routeblobbackfill: iterate %s rows: %w", col.Table, err)
	}
	return nil
}

// encryptOneRow encrypts the plaintext blob (if non-empty) and issues
// the shadow UPDATE. Per-row encrypt/UPDATE failures are tallied in
// res but do not abort the loop — a single corrupt row must not
// prevent the rest of the backlog from migrating.
func (b *Backfiller) encryptOneRow(
	ctx context.Context,
	res *Result,
	col Column,
	id string,
	raw []byte,
	encryptedCounter, updatedCounter *int,
) {
	ct, encErr := routeblob.EncryptJSONBytes(raw, b.encryptor)
	if encErr != nil {
		res.EncryptErrors++
		if b.logger != nil {
			b.logger.Warn("routeblobbackfill: encrypt failed",
				slog.String("table", col.Table),
				slog.String("id", id),
				slog.String("error", encErr.Error()))
		}
		return
	}
	if ct == "" {
		// Empty/null/[] plaintext — nothing to encrypt. Leave the
		// shadow NULL; the read fallback will handle it.
		return
	}

	updateSQL := fmt.Sprintf(
		`UPDATE %q SET %q = $2 WHERE "id" = $1 AND %q IS NULL`,
		col.Table, col.Encrypted, col.Encrypted,
	)
	tag, uErr := b.pool.Exec(ctx, updateSQL, id, ct)
	if uErr != nil {
		res.UpdateErrors++
		if b.logger != nil {
			b.logger.Warn("routeblobbackfill: update failed",
				slog.String("table", col.Table),
				slog.String("id", id),
				slog.String("error", uErr.Error()))
		}
		return
	}
	*encryptedCounter++
	if tag.RowsAffected() > 0 {
		*updatedCounter++
	}
}

// CountPlaintextRemaining reports, per (table, column), the number of
// rows where the plaintext blob is populated but the ciphertext shadow
// is NULL. Keys are `<table>.<plaintext-col>` so the metric label and
// the JSON report agree.
//
// Exported because the gauge wiring needs to call this without owning
// the rest of the Backfiller.
func CountPlaintextRemaining(ctx context.Context, p pool) (map[string]int, error) {
	out := make(map[string]int, len(Columns))
	for _, col := range Columns {
		sql := fmt.Sprintf(
			`SELECT COUNT(*) FROM %q WHERE %q IS NOT NULL AND %q IS NULL`,
			col.Table, col.Plaintext, col.Encrypted,
		)
		var n int
		if err := p.QueryRow(ctx, sql).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s.%s: %w", col.Table, col.Plaintext, err)
		}
		out[col.Plaintext] = n
	}
	return out, nil
}
