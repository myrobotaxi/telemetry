// Package plaintextpurge scrubs the retired plaintext copies of every
// column the telemetry server now stores as ciphertext (MYR-433).
//
// It is the closing step of the MYR-62/63/64 encryption rollout. The
// backfills (accountbackfill, vehiclegpsbackfill, routeblobbackfill)
// sealed each legacy value into its `*Enc` / `*_enc` sibling; the repos
// then stopped reading and writing the plaintext columns. What remains is
// the residue: real Tesla tokens and real coordinates still sitting in
// columns nothing consults. This package removes them.
//
// # Why this is a command and not a migration
//
// Every affected column lives on a Prisma-owned table (Account, Vehicle,
// Drive). docs/architecture/migrations.md §4.2 forbids Go migration SQL
// from referencing those tables at all — CREATE, ALTER, DROP, *or*
// INSERT/UPDATE/DELETE — and contract-guard rule CG-DL-9 enforces it with
// a CI grep over internal/store/migrations/*.sql. A migration file that
// scrubbed "Vehicle" would fail the build.
//
// Application queries against Prisma tables are a different, permitted
// concern (§4.2, governed by data-lifecycle.md §1.4), which is exactly
// what the three backfill commands already are. This package follows that
// established pattern.
//
// Dropping the columns themselves is a react-frontend Prisma migration
// and is deliberately out of scope here; see the MYR-433 PR for the
// reader map and the residual list.
//
// # Verify before destroy
//
// The MYR-230 lesson is that a migration touching production data must
// reckon with the rows already there. A NULL-check on the ciphertext
// sibling is not enough to justify deleting the only other copy of a
// credential: if that ciphertext is corrupt or was sealed under a lost
// key, the plaintext is all there is.
//
// So every row is verified before it is scrubbed. The purge decrypts the
// ciphertext, compares it against the plaintext it is about to destroy,
// and only writes when they agree. A row that cannot be verified is
// counted, logged, and left completely alone — the operator can then fix
// the backfill and re-run. The purge is idempotent and safe to re-run.
package plaintextpurge

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// Kind selects how a column's plaintext is compared against its
// decrypted ciphertext. The three kinds mirror the three encryption
// helpers the rollout used.
type Kind int

const (
	// KindString compares verbatim. Used for the OAuth token columns,
	// which are sealed with cryptox.EncryptString.
	KindString Kind = iota
	// KindFloat parses both sides as float64 before comparing, matching
	// the strconv round-trip that vehicle_gps_encryption.go relies on.
	KindFloat
	// KindJSON compares semantically (decoded shape), not byte-wise:
	// Postgres normalises jsonb whitespace and key order on storage, so
	// the plaintext column's text form legitimately differs from the
	// bytes that were sealed.
	KindJSON
)

// Column describes one retired plaintext column and how to retire it.
type Column struct {
	// Table and Plaintext name the column being scrubbed; Encrypted is
	// the sibling that must verify first.
	Table     string
	Plaintext string
	Encrypted string
	// Kind selects the verification comparison.
	Kind Kind
	// ScrubSQL is the literal the column is set to once verified.
	//
	// It is NOT always NULL. "latitude"/"longitude" are NOT NULL with
	// default 0 on the Prisma schema, and "routePoints" is NOT NULL —
	// scrubbing those to NULL would violate the constraint and abort. The
	// zero coordinate and the empty array are the correct "no data here"
	// values for those columns, and neither reveals a location.
	ScrubSQL string
	// RemainingPredicate matches rows that still hold scrubbable data. It
	// is the definition of "not yet purged" for both the scrub pass and
	// the operator-facing residual count, so the two cannot disagree.
	RemainingPredicate string
}

// Label is the stable `<table>.<column>` key used in results, metrics and
// operator output.
func (c Column) Label() string { return c.Table + "." + c.Plaintext }

// Columns is the full set of plaintext columns MYR-433 retires, in a
// deliberate order: Tesla OAuth tokens first.
//
// The token columns are the sharpest item in the database. Vehicle GPS
// and route blobs reveal where someone went; a Tesla access/refresh token
// grants control of their car. If a purge run is interrupted, the
// credentials should already be gone.
var Columns = []Column{
	{
		Table: "Account", Plaintext: "access_token", Encrypted: "access_token_enc",
		Kind: KindString, ScrubSQL: "NULL",
		RemainingPredicate: `"access_token" IS NOT NULL`,
	},
	{
		Table: "Account", Plaintext: "refresh_token", Encrypted: "refresh_token_enc",
		Kind: KindString, ScrubSQL: "NULL",
		RemainingPredicate: `"refresh_token" IS NOT NULL`,
	},
	{
		Table: "Account", Plaintext: "id_token", Encrypted: "id_token_enc",
		Kind: KindString, ScrubSQL: "NULL",
		RemainingPredicate: `"id_token" IS NOT NULL`,
	},
	// Vehicle GPS. latitude/longitude are NOT NULL default 0, so they
	// scrub to 0 and "remaining" means "not already at the zero
	// coordinate". The nullable destination/origin pairs scrub to NULL.
	{
		Table: "Vehicle", Plaintext: "latitude", Encrypted: "latitudeEnc",
		Kind: KindFloat, ScrubSQL: "0",
		RemainingPredicate: `"latitude" IS NOT NULL AND "latitude" <> 0`,
	},
	{
		Table: "Vehicle", Plaintext: "longitude", Encrypted: "longitudeEnc",
		Kind: KindFloat, ScrubSQL: "0",
		RemainingPredicate: `"longitude" IS NOT NULL AND "longitude" <> 0`,
	},
	{
		Table: "Vehicle", Plaintext: "destinationLatitude", Encrypted: "destinationLatitudeEnc",
		Kind: KindFloat, ScrubSQL: "NULL",
		RemainingPredicate: `"destinationLatitude" IS NOT NULL`,
	},
	{
		Table: "Vehicle", Plaintext: "destinationLongitude", Encrypted: "destinationLongitudeEnc",
		Kind: KindFloat, ScrubSQL: "NULL",
		RemainingPredicate: `"destinationLongitude" IS NOT NULL`,
	},
	{
		Table: "Vehicle", Plaintext: "originLatitude", Encrypted: "originLatitudeEnc",
		Kind: KindFloat, ScrubSQL: "NULL",
		RemainingPredicate: `"originLatitude" IS NOT NULL`,
	},
	{
		Table: "Vehicle", Plaintext: "originLongitude", Encrypted: "originLongitudeEnc",
		Kind: KindFloat, ScrubSQL: "NULL",
		RemainingPredicate: `"originLongitude" IS NOT NULL`,
	},
	{
		Table: "Vehicle", Plaintext: "navRouteCoordinates", Encrypted: "navRouteCoordinatesEnc",
		Kind: KindJSON, ScrubSQL: "NULL",
		RemainingPredicate: `"navRouteCoordinates" IS NOT NULL AND "navRouteCoordinates"::text NOT IN ('[]', 'null')`,
	},
	{
		Table: "Drive", Plaintext: "routePoints", Encrypted: "routePointsEnc",
		Kind: KindJSON, ScrubSQL: `'[]'::jsonb`,
		RemainingPredicate: `"routePoints" IS NOT NULL AND "routePoints"::text NOT IN ('[]', 'null')`,
	},
}

// ColumnResult is the per-column outcome of a purge pass.
type ColumnResult struct {
	// Scanned is how many rows still held scrubbable plaintext.
	Scanned int
	// Purged is how many rows were verified and scrubbed. Always 0 on a
	// dry run.
	Purged int
	// Unsealed counts rows whose ciphertext sibling is absent. Their
	// plaintext is the ONLY copy, so it is left in place — run the
	// matching backfill first.
	Unsealed int
	// Mismatched counts rows whose ciphertext decrypted successfully but
	// did not equal the plaintext. Left in place; needs investigation.
	Mismatched int
	// Undecryptable counts rows whose ciphertext would not decrypt at
	// all — typically a key-rotation error. Left in place.
	Undecryptable int
	// UpdateErrors counts rows that verified but whose scrub UPDATE
	// failed.
	UpdateErrors int
	// Remaining is the post-pass count of rows still matching
	// RemainingPredicate. This is the number that must reach zero for the
	// MYR-433 acceptance bar to hold.
	Remaining int
}

// Blocked reports rows that were left in place because they could not be
// verified. A non-zero total means the purge is incomplete by design and
// the operator has something to fix.
func (c ColumnResult) Blocked() int {
	return c.Unsealed + c.Mismatched + c.Undecryptable
}

// Result aggregates every column's outcome, keyed by Column.Label().
type Result struct {
	Columns map[string]ColumnResult
	DryRun  bool
}

// TotalPurged sums the scrubbed rows across all columns.
func (r Result) TotalPurged() int {
	n := 0
	for _, c := range r.Columns {
		n += c.Purged
	}
	return n
}

// TotalBlocked sums the unverifiable rows across all columns.
func (r Result) TotalBlocked() int {
	n := 0
	for _, c := range r.Columns {
		n += c.Blocked()
	}
	return n
}

// TotalRemaining sums the post-pass residue across all columns. The
// MYR-433 bar is met when this is zero.
func (r Result) TotalRemaining() int {
	n := 0
	for _, c := range r.Columns {
		n += c.Remaining
	}
	return n
}

// UpdateErrors sums scrub-write failures across all columns.
func (r Result) UpdateErrors() int {
	n := 0
	for _, c := range r.Columns {
		n += c.UpdateErrors
	}
	return n
}

// pool is the subset of *pgxpool.Pool the purge uses. A narrow interface
// keeps tests light, matching the backfill packages.
type pool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Purger scrubs verified plaintext residue. Construct via New.
type Purger struct {
	pool      pool
	encryptor cryptox.Encryptor
	logger    *slog.Logger
}

// New returns a Purger bound to the given pool and encryptor.
//
// The encryptor MUST be the same one the server runs with: the purge
// decides whether to destroy data based on what this encryptor can
// decrypt, so a wrong key makes every row unverifiable (and, correctly,
// purges nothing).
//
// Panics on a nil Encryptor — a purge that cannot verify must not run.
func New(p *pgxpool.Pool, enc cryptox.Encryptor, logger *slog.Logger) *Purger {
	if enc == nil {
		panic("plaintextpurge.New: encryptor must not be nil")
	}
	return &Purger{pool: p, encryptor: enc, logger: logger}
}

// Run purges every column in Columns. When dryRun is true the rows are
// scanned and verified but nothing is written, so an operator can see
// exactly what would happen (and what is blocked) before committing.
//
// Per-row failures never abort the run: one corrupt row must not keep the
// rest of the fleet's credentials sitting in plaintext. They are tallied
// in the Result instead, and the caller decides the exit code.
func (p *Purger) Run(ctx context.Context, dryRun bool) (Result, error) {
	res := Result{Columns: make(map[string]ColumnResult, len(Columns)), DryRun: dryRun}

	for _, col := range Columns {
		cr, err := p.purgeColumn(ctx, col, dryRun)
		if err != nil {
			res.Columns[col.Label()] = cr
			return res, err
		}

		remaining, err := countRemaining(ctx, p.pool, col)
		if err != nil {
			res.Columns[col.Label()] = cr
			return res, err
		}
		cr.Remaining = remaining
		res.Columns[col.Label()] = cr

		if p.logger != nil {
			p.logger.Info("plaintextpurge: column complete",
				slog.String("column", col.Label()),
				slog.Bool("dry_run", dryRun),
				slog.Int("scanned", cr.Scanned),
				slog.Int("purged", cr.Purged),
				slog.Int("blocked", cr.Blocked()),
				slog.Int("remaining", cr.Remaining),
			)
		}
	}
	return res, nil
}

// purgeColumn runs one column's verify-then-scrub pass.
//
// The plaintext is pulled as ::text so a single scan path serves floats,
// jsonb and text alike; each Kind reinterprets that text during
// verification.
func (p *Purger) purgeColumn(ctx context.Context, col Column, dryRun bool) (ColumnResult, error) {
	var cr ColumnResult

	selectSQL := fmt.Sprintf(
		`SELECT "id", %q::text, %q FROM %q WHERE %s`,
		col.Plaintext, col.Encrypted, col.Table, col.RemainingPredicate,
	)
	rows, err := p.pool.Query(ctx, selectSQL)
	if err != nil {
		return cr, fmt.Errorf("plaintextpurge: select %s: %w", col.Label(), err)
	}

	type candidate struct {
		id        string
		plaintext string
	}
	var verified []candidate

	for rows.Next() {
		var id string
		var plaintext *string
		var ciphertext *string
		if scanErr := rows.Scan(&id, &plaintext, &ciphertext); scanErr != nil {
			rows.Close()
			return cr, fmt.Errorf("plaintextpurge: scan %s row: %w", col.Label(), scanErr)
		}
		cr.Scanned++

		if plaintext == nil {
			continue
		}
		switch p.verify(col, id, *plaintext, ciphertext) {
		case verdictOK:
			verified = append(verified, candidate{id: id, plaintext: *plaintext})
		case verdictUnsealed:
			cr.Unsealed++
		case verdictMismatch:
			cr.Mismatched++
		case verdictUndecryptable:
			cr.Undecryptable++
		}
	}
	if iterErr := rows.Err(); iterErr != nil {
		rows.Close()
		return cr, fmt.Errorf("plaintextpurge: iterate %s rows: %w", col.Label(), iterErr)
	}
	rows.Close()

	if dryRun {
		return cr, nil
	}

	// Scrub after the cursor is closed rather than inside the iteration:
	// the UPDATE re-checks the ciphertext is still present, and running it
	// against rows a live server may be concurrently writing is safer once
	// we are no longer holding a portal open on the same table.
	scrubSQL := fmt.Sprintf(
		`UPDATE %q SET %q = %s WHERE "id" = $1 AND %q IS NOT NULL`,
		col.Table, col.Plaintext, col.ScrubSQL, col.Encrypted,
	)
	for _, c := range verified {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return cr, fmt.Errorf("plaintextpurge: cancelled: %w", ctxErr)
		}
		if _, uErr := p.pool.Exec(ctx, scrubSQL, c.id); uErr != nil {
			cr.UpdateErrors++
			if p.logger != nil {
				p.logger.Warn("plaintextpurge: scrub failed",
					slog.String("column", col.Label()),
					slog.String("id", c.id),
					slog.String("error", uErr.Error()))
			}
			continue
		}
		cr.Purged++
	}
	return cr, nil
}

// countRemaining reports how many rows still match the column's
// RemainingPredicate — the operator-facing "is this done?" number.
func countRemaining(ctx context.Context, p pool, col Column) (int, error) {
	sql := fmt.Sprintf(`SELECT COUNT(*) FROM %q WHERE %s`, col.Table, col.RemainingPredicate)
	var n int
	if err := p.QueryRow(ctx, sql).Scan(&n); err != nil {
		return 0, fmt.Errorf("plaintextpurge: count %s: %w", col.Label(), err)
	}
	return n, nil
}

// CountRemaining reports the residual plaintext row count for every
// column, keyed by Column.Label(). Exported so verification tests and
// operator tooling can assert the MYR-433 bar without owning a Purger.
func CountRemaining(ctx context.Context, p pool) (map[string]int, error) {
	out := make(map[string]int, len(Columns))
	for _, col := range Columns {
		n, err := countRemaining(ctx, p, col)
		if err != nil {
			return nil, err
		}
		out[col.Label()] = n
	}
	return out, nil
}
