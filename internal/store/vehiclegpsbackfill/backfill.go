// Package vehiclegpsbackfill encrypts the six pre-existing Vehicle GPS
// plaintext columns into their *Enc ciphertext shadows during the
// MYR-63 cross-repo Vehicle-GPS encryption rollout. It is the
// dual-write companion to VehicleRepo.UpdateTelemetry: the repo handles
// new writes; this package handles the legacy backlog.
//
// Idempotent. Re-running over a fully migrated table touches zero rows.
//
// Atomic-pair semantics: the six columns are organized as three pairs
// (`latitude`/`longitude`, `destinationLatitude`/`destinationLongitude`,
// `originLatitude`/`originLongitude`). Every backfill writes a pair as
// a unit — never half. A pre-existing half-pair `*Enc` row is detected
// by CountPlaintextRemaining and reported as plaintext-remaining for
// each populated half; the backfill SELECT then picks it up the next
// run and the half-pair is "completed" by re-encrypting from the
// plaintext source of truth.
//
// The package is intentionally separate from internal/store so the
// running telemetry server doesn't pull in the backfill code path. The
// CLI in cmd/backfill-vehicle-gps/ is the canonical operator entry
// point; it can also be invoked from tests.
package vehiclegpsbackfill

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// Columns is the canonical ordered list of the six plaintext GPS
// columns the dual-write rollout encrypts. Exported so the gauge
// wiring labels by the same column names.
var Columns = []string{
	"latitude",
	"longitude",
	"destinationLatitude",
	"destinationLongitude",
	"originLatitude",
	"originLongitude",
}

// Result reports the outcome of a Run. Counts are tallied in-process
// and are independent of the post-run plaintext-remaining check.
type Result struct {
	// RowsScanned is the number of Vehicle rows the SELECT returned.
	RowsScanned int
	// PairsEncrypted is the number of (lat, lng) pairs the run wrote
	// (one row can contribute up to 3).
	PairsEncrypted int
	// RowsUpdated is the number of distinct rows the UPDATE touched.
	RowsUpdated int
	// EncryptErrors is the number of pairs skipped because cryptox
	// failed.
	EncryptErrors int
	// UpdateErrors is the number of rows skipped because UPDATE
	// failed.
	UpdateErrors int
	// PlaintextRemaining is the post-run snapshot of how many rows
	// still hold plaintext-without-ciphertext per column. The rollout
	// is complete when every value is zero.
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

// Backfiller runs the legacy → ciphertext migration for Vehicle rows.
// Construct via New; the zero value is unusable.
type Backfiller struct {
	pool      pool
	encryptor cryptox.Encryptor
	logger    *slog.Logger
}

// New returns a Backfiller bound to the given pool + encryptor. The
// encryptor MUST be the same one wired into VehicleRepo so newly
// encrypted rows are decryptable by the running server.
//
// Panics on a nil Encryptor — mirrors store.NewVehicleRepoWithEncryption
// so the dual-write contract fails loud at construction.
func New(p *pgxpool.Pool, enc cryptox.Encryptor, logger *slog.Logger) *Backfiller {
	if enc == nil {
		panic("vehiclegpsbackfill.New: encryptor must not be nil")
	}
	return &Backfiller{pool: p, encryptor: enc, logger: logger}
}

// Run scans every Vehicle row that holds at least one plaintext-
// without-ciphertext GPS pair, encrypts the missing values, and
// updates the row. Returns a Result regardless of whether any
// individual row failed; the caller decides whether to exit non-zero
// based on Result.Errors().
func (b *Backfiller) Run(ctx context.Context) (Result, error) {
	res := Result{PlaintextRemaining: map[string]int{}}
	batch, firstErr := b.collectBatch(ctx, &res)
	if batch == nil && firstErr != nil {
		return res, firstErr
	}
	if uErr := b.applyBatch(ctx, batch, &res); uErr != nil && firstErr == nil {
		firstErr = uErr
	}
	if remaining, rErr := CountPlaintextRemaining(ctx, b.pool); rErr != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("count plaintext remaining: %w", rErr)
		}
	} else {
		res.PlaintextRemaining = remaining
	}
	return res, firstErr
}

// CountPlaintextRemaining reports, per GPS column, the number of
// Vehicle rows where `<col>` is non-NULL and `<col>Enc` is NULL — i.e.
// rows that still hold plaintext without ciphertext. Used by the CLI's
// post-run report and by the running server's periodic gauge update.
//
// Exported because the gauge wiring needs to call this without owning
// the rest of the Backfiller.
//
// Note on the main `latitude`/`longitude` pair: those columns are
// non-NULL Float with default 0, so "non-NULL" is true for every row
// in the table. The metric for those two columns therefore counts
// "rows whose latitudeEnc/longitudeEnc shadow is NULL", which is the
// rollout-completion signal we want.
func CountPlaintextRemaining(ctx context.Context, p pool) (map[string]int, error) {
	out := make(map[string]int, len(Columns))
	for _, col := range Columns {
		// Column names are constants, not user input — safe to interpolate.
		sql := fmt.Sprintf(
			`SELECT COUNT(*) FROM "Vehicle" WHERE %q IS NOT NULL AND %q IS NULL`,
			col, col+"Enc",
		)
		var n int
		if err := p.QueryRow(ctx, sql).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", col, err)
		}
		out[col] = n
	}
	return out, nil
}

// formatFloat encodes f using the lossless round-trip representation
// that matches the TS String(x). Centralised so all encoders agree.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
