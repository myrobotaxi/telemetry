// Package accountbackfill encrypts pre-existing Account.<col> plaintext
// rows into the matching Account.<col>_enc ciphertext column during the
// MYR-62 cross-repo encryption rollout. It is the dual-write companion
// to AccountRepo.UpdateTeslaToken: the repo handles new writes; this
// package handles the legacy backlog.
//
// Idempotent. Re-running over a fully migrated table touches zero rows.
// Mixed-state rows (some columns encrypted, others not) are recoverable:
// each column is filled independently, NULLs are passed for already-
// encrypted columns, and the SQL UPDATE uses COALESCE($n, "<col>_enc")
// so partial fills compose monotonically.
//
// The package is intentionally separate from internal/store so the
// running telemetry server doesn't pull in the backfill code path. The
// CLI in cmd/backfill-account-tokens/ is the canonical operator entry
// point; it can also be invoked from tests.
package accountbackfill

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tnando/my-robo-taxi-telemetry/internal/cryptox"
)

// TokenColumns is the canonical ordered list of plaintext token columns
// the dual-write rollout encrypts. Exported so the gauge wiring in
// internal/store/accountmetrics can label by the same column names.
var TokenColumns = []string{"access_token", "refresh_token", "id_token"}

// Result reports the outcome of a Run. Counts are tallied in-process and
// are independent of the post-run plaintext-remaining check below.
type Result struct {
	// RowsScanned is the number of Account rows the SELECT returned.
	RowsScanned int
	// ColumnsEncrypted is the number of individual *_enc cells the run
	// wrote (one row can contribute up to 3).
	ColumnsEncrypted int
	// RowsUpdated is the number of distinct rows the UPDATE touched.
	RowsUpdated int
	// EncryptErrors is the number of rows skipped because cryptox.Encrypt
	// failed. The first such error is returned by Run.
	EncryptErrors int
	// UpdateErrors is the number of rows skipped because the UPDATE
	// failed. The first such error is returned by Run.
	UpdateErrors int
	// PlaintextRemaining is the post-run snapshot of how many rows still
	// hold plaintext-without-ciphertext per column. The rollout is
	// complete when every value is zero.
	PlaintextRemaining map[string]int
}

// Errors reports whether any row failed mid-run. Used by the CLI's
// non-zero exit decision.
func (r Result) Errors() int { return r.EncryptErrors + r.UpdateErrors }

// pool is the subset of *pgxpool.Pool the backfill uses. Defining a
// narrow interface lets tests (in the same package) mock it without
// pulling in the full pgx surface. Production callers always pass a
// real pool.
type pool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Backfiller runs the legacy → ciphertext migration for Account rows.
// Construct via New; the zero value is unusable.
type Backfiller struct {
	pool      pool
	encryptor cryptox.Encryptor
	logger    *slog.Logger
}

// New returns a Backfiller bound to the given pool + encryptor. The
// encryptor MUST be the same one wired into AccountRepo so newly
// encrypted rows are decryptable by the running server.
func New(p *pgxpool.Pool, enc cryptox.Encryptor, logger *slog.Logger) *Backfiller {
	return &Backfiller{pool: p, encryptor: enc, logger: logger}
}

// pending captures one row's pre-update state: the columns we intend to
// fill (nil where there's nothing to write). The COALESCE in the
// UPDATE statement skips nil columns so a partially encrypted row
// doesn't overwrite already-good ciphertext.
type pending struct {
	id        string
	accessCT  *string
	refreshCT *string
	idCT      *string
}

// hasWork reports whether the row needs an UPDATE. A row with no
// pending columns is left out of the batch entirely.
func (p pending) hasWork() bool {
	return p.accessCT != nil || p.refreshCT != nil || p.idCT != nil
}

// Run scans every Tesla Account row that holds plaintext-without-
// ciphertext in any of the three token columns, encrypts the missing
// values, and updates the row. Returns a Result regardless of whether
// any individual row failed; the caller decides whether to exit non-zero
// based on Result.Errors().
//
// The SELECT and per-row UPDATE run inside the caller-provided context;
// cancelling ctx aborts the loop and returns whatever was processed so
// far.
func (b *Backfiller) Run(ctx context.Context) (Result, error) {
	res := Result{PlaintextRemaining: map[string]int{}}
	batch, firstErr := b.collectBatch(ctx, &res)
	if batch == nil && firstErr != nil {
		// collectBatch nils the batch only when the failure is fatal
		// (SELECT or scan row); soft encrypt errors return a non-nil
		// (possibly empty) batch alongside firstErr.
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

// collectBatch executes the SELECT and converts each scanned row into a
// pending update. A nil returned slice means the SELECT/scan itself
// failed (fatal); a non-nil slice with a non-nil error means soft
// encryption failures during the scan loop (recoverable; some rows
// still ready to UPDATE).
func (b *Backfiller) collectBatch(ctx context.Context, res *Result) ([]pending, error) {
	const selectSQL = `SELECT "id",
            "access_token",  "access_token_enc",
            "refresh_token", "refresh_token_enc",
            "id_token",      "id_token_enc"
        FROM "Account"
        WHERE "provider" = 'tesla'
          AND (
              ("access_token"  IS NOT NULL AND "access_token_enc"  IS NULL)
           OR ("refresh_token" IS NOT NULL AND "refresh_token_enc" IS NULL)
           OR ("id_token"      IS NOT NULL AND "id_token_enc"      IS NULL)
          )`
	rows, err := b.pool.Query(ctx, selectSQL)
	if err != nil {
		return nil, fmt.Errorf("accountbackfill: select rows: %w", err)
	}
	defer rows.Close()

	batch := make([]pending, 0)
	var firstErr error
	for rows.Next() {
		var (
			id                  string
			accessPT, accessEnc *string
			refPT, refEnc       *string
			idPT, idEnc         *string
		)
		if err := rows.Scan(&id, &accessPT, &accessEnc, &refPT, &refEnc, &idPT, &idEnc); err != nil {
			return nil, fmt.Errorf("accountbackfill: scan row: %w", err)
		}
		res.RowsScanned++
		p, pErr := b.encryptRow(id, accessPT, accessEnc, refPT, refEnc, idPT, idEnc, res)
		if pErr != nil && firstErr == nil {
			firstErr = pErr
		}
		if p.hasWork() {
			batch = append(batch, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("accountbackfill: iterate rows: %w", err)
	}
	return batch, firstErr
}

// encryptRow encrypts whichever plaintext-without-ciphertext columns the
// row exposes. Tally counters are updated on res; the first encryption
// failure is returned. A partial row (one column failed, others
// succeeded) still surfaces those that succeeded.
func (b *Backfiller) encryptRow(
	id string,
	accessPT, accessEnc, refPT, refEnc, idPT, idEnc *string,
	res *Result,
) (pending, error) {
	p := pending{id: id}
	var firstErr error
	for _, col := range []struct {
		name    string
		pt, enc *string
		dst     **string
	}{
		{"access_token", accessPT, accessEnc, &p.accessCT},
		{"refresh_token", refPT, refEnc, &p.refreshCT},
		{"id_token", idPT, idEnc, &p.idCT},
	} {
		if col.pt == nil || (col.enc != nil && *col.enc != "") {
			continue
		}
		ct, err := b.encryptor.EncryptString(*col.pt)
		if err != nil {
			res.EncryptErrors++
			if firstErr == nil {
				// col.name is one of TokenColumns (access_token,
				// refresh_token, id_token) — token *names*, not values.
				// CG-DC-2 contract-guard scans for these substrings; the
				// safer phrasing is to refer to the column index.
				firstErr = fmt.Errorf("encrypt column %q (id=%s): %w", col.name, id, err)
			}
			continue
		}
		ctCopy := ct
		*col.dst = &ctCopy
		res.ColumnsEncrypted++
	}
	return p, firstErr
}

// applyBatch issues the per-row UPDATE for every pending entry. Returns
// the first UPDATE error (if any); subsequent failures are tallied in
// res.UpdateErrors but don't short-circuit the loop, so a single bad
// row doesn't strand the rest.
func (b *Backfiller) applyBatch(ctx context.Context, batch []pending, res *Result) error {
	const updateSQL = `UPDATE "Account"
SET "access_token_enc"  = COALESCE($2, "access_token_enc"),
    "refresh_token_enc" = COALESCE($3, "refresh_token_enc"),
    "id_token_enc"      = COALESCE($4, "id_token_enc")
WHERE "id" = $1`
	var firstErr error
	for _, p := range batch {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("accountbackfill: cancelled: %w", ctxErr)
		}
		tag, uErr := b.pool.Exec(ctx, updateSQL, p.id, p.accessCT, p.refreshCT, p.idCT)
		if uErr != nil {
			res.UpdateErrors++
			if firstErr == nil {
				firstErr = fmt.Errorf("update row id=%s: %w", p.id, uErr)
			}
			if b.logger != nil {
				b.logger.Warn("accountbackfill: update failed", slog.String("id", p.id), slog.String("error", uErr.Error()))
			}
			continue
		}
		if tag.RowsAffected() > 0 {
			res.RowsUpdated++
		}
	}
	return firstErr
}

// CountPlaintextRemaining reports, per token column, the number of Tesla
// Account rows where `<col>` is non-NULL and `<col>_enc` is NULL — i.e.
// rows that still hold plaintext without ciphertext. Used by the CLI's
// post-run report and by the running server's periodic gauge update.
//
// Exported because the gauge wiring in cmd/telemetry-server/ needs to
// call this without owning the rest of the Backfiller.
func CountPlaintextRemaining(ctx context.Context, p pool) (map[string]int, error) {
	out := make(map[string]int, len(TokenColumns))
	for _, col := range TokenColumns {
		// Column names are constants, not user input: safe to interpolate.
		sql := fmt.Sprintf(
			`SELECT COUNT(*) FROM "Account"
             WHERE "provider" = 'tesla' AND %q IS NOT NULL AND %q IS NULL`,
			col, col+"_enc",
		)
		var n int
		if err := p.QueryRow(ctx, sql).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", col, err)
		}
		out[col] = n
	}
	return out, nil
}

