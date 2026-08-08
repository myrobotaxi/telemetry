// Package locationlabelbackfill seals the pre-existing plaintext
// geocoded location labels into their `*Enc` ciphertext shadows for the
// MYR-447 rollout (NFR-3.23).
//
// Eight columns across two Prisma-owned tables:
//
//   - Vehicle.locationName / locationAddress — where the car is now, as a
//     place name and a street address.
//   - Vehicle.destinationName / destinationAddress — where it is headed
//     (navigation atomic group).
//   - Drive.startLocation / startAddress / endLocation / endAddress — the
//     endpoints of a recorded drive.
//
// MYR-433 sealed the COORDINATES for these same rows. It left the
// human-readable rendering of them behind, which is the more sensitive
// half: an operator reading "1600 Pennsylvania Ave NW" learns in one
// glance what a latitude/longitude pair only yields to someone with a
// map. This package is the legacy-backlog companion to VehicleRepo /
// DriveRepo, which handle new writes.
//
// Idempotent by construction. The selection predicate is
//
//	<plaintext> IS NOT NULL AND <plaintext> <> '' AND <enc> IS NULL
//
// and the UPDATE re-asserts the `<enc> IS NULL` half, so a row sealed by
// a concurrent run (or by a previous pass) is skipped rather than
// re-sealed under a fresh nonce. Re-running over a fully migrated table
// touches zero rows.
//
// The package is intentionally separate from internal/store so the
// running telemetry server doesn't pull in the backfill code path. The
// CLI in cmd/backfill-location-labels/ is the canonical operator entry
// point; it can also be invoked from tests.
package locationlabelbackfill

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// Column is a (table, plaintext column, ciphertext column) triple.
type Column struct {
	Table     string
	Plaintext string
	Encrypted string
}

// Key is the stable `<table>.<plaintext-col>` label used in the JSON
// report and in the remaining-count map, so a report line and a metric
// label can never disagree.
func (c Column) Key() string { return c.Table + "." + c.Plaintext }

// Columns is the canonical iteration order: Vehicle before Drive, and
// within each table the same order the SELECT projections use. Stable
// ordering keeps operator output diffable between runs.
var Columns = []Column{
	{Table: "Vehicle", Plaintext: "locationName", Encrypted: "locationNameEnc"},
	{Table: "Vehicle", Plaintext: "locationAddress", Encrypted: "locationAddressEnc"},
	{Table: "Vehicle", Plaintext: "destinationName", Encrypted: "destinationNameEnc"},
	{Table: "Vehicle", Plaintext: "destinationAddress", Encrypted: "destinationAddressEnc"},
	{Table: "Drive", Plaintext: "startLocation", Encrypted: "startLocationEnc"},
	{Table: "Drive", Plaintext: "startAddress", Encrypted: "startAddressEnc"},
	{Table: "Drive", Plaintext: "endLocation", Encrypted: "endLocationEnc"},
	{Table: "Drive", Plaintext: "endAddress", Encrypted: "endAddressEnc"},
}

// ColumnResult is one column's outcome.
type ColumnResult struct {
	// RowsScanned is how many rows the selection predicate matched.
	RowsScanned int
	// RowsUpdated is how many of those the UPDATE actually touched. It
	// can be lower than RowsScanned when a concurrent run or a live
	// server sealed the row between the SELECT and the UPDATE — the
	// `IS NULL` guard makes that a skip, not an overwrite.
	RowsUpdated int
	// EncryptErrors counts rows whose plaintext would not encrypt.
	EncryptErrors int
	// UpdateErrors counts rows whose UPDATE failed.
	UpdateErrors int
}

// Result reports the outcome of a Run.
type Result struct {
	// Columns is keyed by Column.Key().
	Columns map[string]ColumnResult
	// RowsScanned / RowsUpdated / EncryptErrors / UpdateErrors are the
	// cross-column totals, so the CLI's exit decision and the headline
	// numbers don't have to re-derive them.
	RowsScanned   int
	RowsUpdated   int
	EncryptErrors int
	UpdateErrors  int
	// PlaintextRemaining is the post-run snapshot, keyed by Column.Key():
	// rows whose plaintext label is populated but whose ciphertext shadow
	// is still NULL. This is the number that must reach zero before
	// cmd/purge-plaintext-columns can finish the job.
	PlaintextRemaining map[string]int
}

// Errors reports whether any row failed mid-run. Drives the CLI's
// non-zero exit.
func (r Result) Errors() int { return r.EncryptErrors + r.UpdateErrors }

// pool is the subset of *pgxpool.Pool the backfill uses. A narrow
// interface keeps tests light, matching the sibling backfill packages.
type pool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Backfiller seals legacy plaintext labels. Construct via New; the zero
// value is unusable.
type Backfiller struct {
	pool      pool
	encryptor cryptox.Encryptor
	logger    *slog.Logger
}

// New returns a Backfiller bound to the given pool + encryptor. The
// encryptor MUST be the same one wired into the running server, or the
// sealed values will be unreadable by every read path.
//
// Panics on a nil Encryptor — a backfill that cannot seal must not run.
func New(p *pgxpool.Pool, enc cryptox.Encryptor, logger *slog.Logger) *Backfiller {
	if enc == nil {
		panic("locationlabelbackfill.New: encryptor must not be nil")
	}
	return &Backfiller{pool: p, encryptor: enc, logger: logger}
}

// Run seals every unsealed label across all eight columns and returns a
// Result regardless of per-row failures; the caller decides whether to
// exit non-zero based on Result.Errors().
func (b *Backfiller) Run(ctx context.Context) (Result, error) {
	res := Result{
		Columns:            make(map[string]ColumnResult, len(Columns)),
		PlaintextRemaining: map[string]int{},
	}

	for _, col := range Columns {
		cr, err := b.runOneColumn(ctx, col)
		res.Columns[col.Key()] = cr
		res.RowsScanned += cr.RowsScanned
		res.RowsUpdated += cr.RowsUpdated
		res.EncryptErrors += cr.EncryptErrors
		res.UpdateErrors += cr.UpdateErrors
		if err != nil {
			return res, err
		}
		if b.logger != nil {
			b.logger.Info("locationlabelbackfill: column complete",
				slog.String("column", col.Key()),
				slog.Int("scanned", cr.RowsScanned),
				slog.Int("updated", cr.RowsUpdated),
				slog.Int("encrypt_errors", cr.EncryptErrors),
				slog.Int("update_errors", cr.UpdateErrors))
		}
	}

	remaining, rErr := CountPlaintextRemaining(ctx, b.pool)
	if rErr != nil {
		return res, fmt.Errorf("count plaintext remaining: %w", rErr)
	}
	res.PlaintextRemaining = remaining
	return res, nil
}

// runOneColumn seals every row whose plaintext label is non-empty and
// whose ciphertext shadow is NULL.
//
// The empty string is excluded from the selection rather than sealed to
// the absent sentinel: an empty label carries no location, and sealing it
// would flip the `*Enc` column non-NULL, which would permanently hide the
// row from queryDriveMissingAddresses' `IS NULL` discovery predicate
// while recording nothing at all.
func (b *Backfiller) runOneColumn(ctx context.Context, col Column) (ColumnResult, error) {
	var cr ColumnResult

	selectSQL := fmt.Sprintf(
		`SELECT "id", %q FROM %q WHERE %q IS NOT NULL AND %q <> '' AND %q IS NULL`,
		col.Plaintext, col.Table, col.Plaintext, col.Plaintext, col.Encrypted,
	)
	rows, err := b.pool.Query(ctx, selectSQL)
	if err != nil {
		return cr, fmt.Errorf("locationlabelbackfill: select %s: %w", col.Key(), err)
	}

	type pending struct {
		id    string
		plain string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if scanErr := rows.Scan(&p.id, &p.plain); scanErr != nil {
			rows.Close()
			return cr, fmt.Errorf("locationlabelbackfill: scan %s row: %w", col.Key(), scanErr)
		}
		cr.RowsScanned++
		todo = append(todo, p)
	}
	if iterErr := rows.Err(); iterErr != nil {
		rows.Close()
		return cr, fmt.Errorf("locationlabelbackfill: iterate %s rows: %w", col.Key(), iterErr)
	}
	rows.Close()

	// Seal after the cursor is closed rather than inside the iteration:
	// the UPDATE re-checks the shadow is still NULL, and running it
	// against rows a live server may be concurrently writing is safer
	// once we are no longer holding a portal open on the same table.
	for _, p := range todo {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return cr, fmt.Errorf("locationlabelbackfill: cancelled: %w", ctxErr)
		}
		b.sealOneRow(ctx, col, p.id, p.plain, &cr)
	}
	return cr, nil
}

// sealOneRow encrypts one label and issues the guarded UPDATE. Per-row
// failures are tallied and the loop continues — one corrupt row must not
// leave the rest of the fleet's addresses readable.
//
// The label itself is NEVER logged. These columns are P1, "Log-safe: No"
// per docs/contracts/data-classification.md §1.4; a failure line carrying
// the value would recreate the exposure in the log aggregator, which is
// the one place a backfill is guaranteed to be noticed.
func (b *Backfiller) sealOneRow(ctx context.Context, col Column, id, plain string, cr *ColumnResult) {
	ct, encErr := b.encryptor.EncryptString(plain)
	if encErr != nil {
		cr.EncryptErrors++
		if b.logger != nil {
			b.logger.Warn("locationlabelbackfill: encrypt failed",
				slog.String("column", col.Key()),
				slog.String("id", id),
				slog.String("error", encErr.Error()))
		}
		return
	}
	if ct == "" {
		// Unreachable: the selection predicate excludes the empty string,
		// which is the only input EncryptString maps to "". Guard anyway
		// so a future predicate change cannot quietly write an empty
		// ciphertext over the absent sentinel.
		return
	}

	updateSQL := fmt.Sprintf(
		`UPDATE %q SET %q = $2 WHERE "id" = $1 AND %q IS NULL`,
		col.Table, col.Encrypted, col.Encrypted,
	)
	tag, uErr := b.pool.Exec(ctx, updateSQL, id, ct)
	if uErr != nil {
		cr.UpdateErrors++
		if b.logger != nil {
			b.logger.Warn("locationlabelbackfill: update failed",
				slog.String("column", col.Key()),
				slog.String("id", id),
				slog.String("error", uErr.Error()))
		}
		return
	}
	if tag.RowsAffected() > 0 {
		cr.RowsUpdated++
	}
}

// CountPlaintextRemaining reports, per column, how many rows hold a
// non-empty plaintext label with no ciphertext shadow. Keys are
// Column.Key().
//
// Exported so verification tests and operator tooling can assert the
// MYR-447 bar without owning a Backfiller.
func CountPlaintextRemaining(ctx context.Context, p pool) (map[string]int, error) {
	out := make(map[string]int, len(Columns))
	for _, col := range Columns {
		sql := fmt.Sprintf(
			`SELECT COUNT(*) FROM %q WHERE %q IS NOT NULL AND %q <> '' AND %q IS NULL`,
			col.Table, col.Plaintext, col.Plaintext, col.Encrypted,
		)
		var n int
		if err := p.QueryRow(ctx, sql).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", col.Key(), err)
		}
		out[col.Key()] = n
	}
	return out, nil
}
