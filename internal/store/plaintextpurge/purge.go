// Package plaintextpurge scrubs the retired plaintext copies of every
// column the telemetry server now stores as ciphertext (MYR-433,
// MYR-447).
//
// It is the closing step of the MYR-62/63/64 encryption rollout and of
// the MYR-447 label rollout that finished it. The backfills
// (accountbackfill, vehiclegpsbackfill, routeblobbackfill,
// locationlabelbackfill) sealed each legacy value into its `*Enc` /
// `*_enc` sibling; the repos then stopped reading and writing the
// plaintext columns. What remains is the residue: real Tesla tokens, real
// coordinates and the street addresses those coordinates resolve to,
// still sitting in columns nothing consults. This package removes them.
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
// # What makes a row safe to scrub
//
// The MYR-230 lesson is that a migration touching production data must
// reckon with the rows already there. So the purge never removes a value
// on the strength of a NULL-check alone: it decrypts the ciphertext
// sibling first, and a row it cannot decrypt is left completely intact.
//
// The subtle part is what to do when the ciphertext decrypts to something
// DIFFERENT from the plaintext. The first cut of this package treated
// that as a mismatch and refused to scrub — which was exactly backwards,
// and would have made the whole tool useless on a live database:
//
//   - After the ciphertext-only server deploys, plaintext stops being
//     written while ciphertext keeps advancing on every telemetry frame,
//     token refresh and route flush.
//   - So every ACTIVE vehicle, drive and account drifts apart within
//     seconds, and under the old rule every one of them would be refused
//     forever. The only rows that would purge cleanly are dead ones.
//   - Re-running would not help: the backfills only populate a NULL
//     ciphertext, so nothing ever re-converges the two copies.
//
// The correct reading is the opposite. Once nothing writes plaintext, a
// plaintext value that differs from its ciphertext is STALE BY
// CONSTRUCTION — it is a strictly older snapshot of the same field — and
// a cleanly-decrypting ciphertext is proof that a good, current copy
// exists. Stale is precisely what we came to delete.
//
// The rule is therefore: **decryptable ⇒ scrub.** Equality is recorded
// (Redundant vs Stale) for operator visibility, not used as a gate. Only
// two states block: no ciphertext at all (the plaintext may be the only
// copy — run the backfill first), and a ciphertext that will not decrypt
// (typically a key mistake). Both are counted, logged without their
// values, and left untouched. The purge is idempotent and safe to re-run.
package plaintextpurge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
)

// Kind selects how a member's ciphertext is unsealed and compared. The
// three kinds mirror the three encryption helpers the rollout used.
type Kind int

const (
	// KindString is sealed with cryptox.EncryptString and compared
	// verbatim. Used for the OAuth token columns.
	KindString Kind = iota
	// KindFloat parses both sides as float64 before comparing, matching
	// the strconv round-trip vehicle_gps_encryption.go relies on.
	KindFloat
	// KindJSON compares semantically (decoded shape), not byte-wise:
	// Postgres normalises jsonb whitespace and key order on storage, so
	// the plaintext column's text form legitimately differs from the
	// bytes that were sealed.
	KindJSON
)

// Member is one retired plaintext column and the ciphertext that
// replaced it.
type Member struct {
	Plaintext string
	Encrypted string
	Kind      Kind

	// ScrubSQL is the literal the column is set to once verified.
	//
	// It is NOT always NULL. "latitude"/"longitude" are NOT NULL with
	// default 0 on the Prisma schema, and "routePoints" is NOT NULL —
	// scrubbing those to NULL would violate the constraint and abort. The
	// zero coordinate and the empty array are the correct "no data here"
	// values for those columns, and neither reveals a location.
	ScrubSQL string

	// HasData matches rows where this column still holds something worth
	// scrubbing. It is the definition of "not yet purged" for this
	// member, shared by the scan, the scrub and the residual count so
	// the three cannot disagree.
	HasData string
}

// Target is a unit of purging: one or more columns that must be decided
// together.
//
// Most targets hold a single member. The three GPS pairs hold two,
// because a latitude without its longitude is a broken row: if one half
// verifies and the other does not, scrubbing only the verified half would
// leave a readable coordinate behind and half-destroy the pair. A target
// is all-or-nothing — every member must be scrubbable, or none is.
type Target struct {
	Table   string
	Name    string
	Members []Member
}

// Label is the stable `<table>.<name>` key used in results, logs and
// operator output.
func (t Target) Label() string { return t.Table + "." + t.Name }

// hasDataPredicate matches rows where ANY member still holds scrubbable
// data.
func (t Target) hasDataPredicate() string {
	parts := make([]string, 0, len(t.Members))
	for _, m := range t.Members {
		parts = append(parts, "("+m.HasData+")")
	}
	return strings.Join(parts, " OR ")
}

// Targets is the full set of plaintext columns MYR-433 and MYR-447
// retire, in a deliberate order: Tesla OAuth tokens first.
//
// The token columns are the sharpest item in the database. Vehicle GPS
// and route blobs reveal where someone went; a Tesla access/refresh token
// grants control of their car. If a purge run is interrupted, the
// credentials should already be gone.
var Targets = []Target{
	{Table: "Account", Name: "access_token", Members: []Member{
		{Plaintext: "access_token", Encrypted: "access_token_enc", Kind: KindString,
			ScrubSQL: "NULL", HasData: `"access_token" IS NOT NULL`},
	}},
	{Table: "Account", Name: "refresh_token", Members: []Member{
		{Plaintext: "refresh_token", Encrypted: "refresh_token_enc", Kind: KindString,
			ScrubSQL: "NULL", HasData: `"refresh_token" IS NOT NULL`},
	}},
	{Table: "Account", Name: "id_token", Members: []Member{
		{Plaintext: "id_token", Encrypted: "id_token_enc", Kind: KindString,
			ScrubSQL: "NULL", HasData: `"id_token" IS NOT NULL`},
	}},

	// The three GPS pairs. Each is one target so a half-verifying pair
	// cannot leave one readable coordinate behind.
	{Table: "Vehicle", Name: "latitude+longitude", Members: []Member{
		{Plaintext: "latitude", Encrypted: "latitudeEnc", Kind: KindFloat,
			ScrubSQL: "0", HasData: `"latitude" IS NOT NULL AND "latitude" <> 0`},
		{Plaintext: "longitude", Encrypted: "longitudeEnc", Kind: KindFloat,
			ScrubSQL: "0", HasData: `"longitude" IS NOT NULL AND "longitude" <> 0`},
	}},
	{Table: "Vehicle", Name: "destinationLatitude+destinationLongitude", Members: []Member{
		{Plaintext: "destinationLatitude", Encrypted: "destinationLatitudeEnc", Kind: KindFloat,
			ScrubSQL: "NULL", HasData: `"destinationLatitude" IS NOT NULL`},
		{Plaintext: "destinationLongitude", Encrypted: "destinationLongitudeEnc", Kind: KindFloat,
			ScrubSQL: "NULL", HasData: `"destinationLongitude" IS NOT NULL`},
	}},
	{Table: "Vehicle", Name: "originLatitude+originLongitude", Members: []Member{
		{Plaintext: "originLatitude", Encrypted: "originLatitudeEnc", Kind: KindFloat,
			ScrubSQL: "NULL", HasData: `"originLatitude" IS NOT NULL`},
		{Plaintext: "originLongitude", Encrypted: "originLongitudeEnc", Kind: KindFloat,
			ScrubSQL: "NULL", HasData: `"originLongitude" IS NOT NULL`},
	}},

	{Table: "Vehicle", Name: "navRouteCoordinates", Members: []Member{
		{Plaintext: "navRouteCoordinates", Encrypted: "navRouteCoordinatesEnc", Kind: KindJSON,
			ScrubSQL: "NULL",
			HasData:  `"navRouteCoordinates" IS NOT NULL AND "navRouteCoordinates"::text NOT IN ('[]', 'null')`},
	}},
	{Table: "Drive", Name: "routePoints", Members: []Member{
		{Plaintext: "routePoints", Encrypted: "routePointsEnc", Kind: KindJSON,
			ScrubSQL: `'[]'::jsonb`,
			HasData:  `"routePoints" IS NOT NULL AND "routePoints"::text NOT IN ('[]', 'null')`},
	}},

	// MYR-447 — the geocoded location labels. Each is its own target,
	// deliberately NOT grouped the way the GPS pairs are.
	//
	// A GPS pair is grouped because a latitude without its longitude is a
	// broken row: scrubbing the verified half would both leave a readable
	// coordinate and half-destroy the pair, so refusing is strictly
	// better. Labels have neither property. Each one is an independently
	// recoverable string, and each one on its own already gives away the
	// location — "Alcatraz Landing" and "Pier 33, San Francisco" are two
	// spellings of the same disclosure, not two halves of one. Grouping
	// them would mean one undecryptable column pins the other three in
	// plaintext, which is the wrong trade in both directions: scrubbing
	// what verifies strictly reduces exposure and destroys nothing.
	//
	// ScrubSQL tracks each column's nullability in the react-frontend
	// Prisma schema, because an all-or-nothing UPDATE that violates a NOT
	// NULL constraint aborts the whole target:
	//
	//	Vehicle.locationName        String  @default("")  → NOT NULL → ''
	//	Vehicle.locationAddress     String  @default("")  → NOT NULL → ''
	//	Vehicle.destinationName     String?               → nullable → NULL
	//	Vehicle.destinationAddress  String?               → nullable → NULL
	//	Drive.startLocation         String                → NOT NULL → ''
	//	Drive.startAddress          String                → NOT NULL → ''
	//	Drive.endLocation           String                → NOT NULL → ''
	//	Drive.endAddress            String                → NOT NULL → ''
	//
	// The empty string is the correct "no data here" value for the NOT
	// NULL columns and reveals no location, exactly as the zero
	// coordinate does for latitude/longitude.
	//
	// HasData excludes the empty string as well as NULL: an empty label
	// discloses nothing, so churning those rows would inflate the
	// operator's counters with work that changes nothing.
	{Table: "Vehicle", Name: "locationName", Members: []Member{
		{Plaintext: "locationName", Encrypted: "locationNameEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"locationName" IS NOT NULL AND "locationName" <> ''`},
	}},
	{Table: "Vehicle", Name: "locationAddress", Members: []Member{
		{Plaintext: "locationAddress", Encrypted: "locationAddressEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"locationAddress" IS NOT NULL AND "locationAddress" <> ''`},
	}},
	{Table: "Vehicle", Name: "destinationName", Members: []Member{
		{Plaintext: "destinationName", Encrypted: "destinationNameEnc", Kind: KindString,
			ScrubSQL: "NULL", HasData: `"destinationName" IS NOT NULL AND "destinationName" <> ''`},
	}},
	{Table: "Vehicle", Name: "destinationAddress", Members: []Member{
		{Plaintext: "destinationAddress", Encrypted: "destinationAddressEnc", Kind: KindString,
			ScrubSQL: "NULL", HasData: `"destinationAddress" IS NOT NULL AND "destinationAddress" <> ''`},
	}},
	{Table: "Drive", Name: "startLocation", Members: []Member{
		{Plaintext: "startLocation", Encrypted: "startLocationEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"startLocation" IS NOT NULL AND "startLocation" <> ''`},
	}},
	{Table: "Drive", Name: "startAddress", Members: []Member{
		{Plaintext: "startAddress", Encrypted: "startAddressEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"startAddress" IS NOT NULL AND "startAddress" <> ''`},
	}},
	{Table: "Drive", Name: "endLocation", Members: []Member{
		{Plaintext: "endLocation", Encrypted: "endLocationEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"endLocation" IS NOT NULL AND "endLocation" <> ''`},
	}},
	{Table: "Drive", Name: "endAddress", Members: []Member{
		{Plaintext: "endAddress", Encrypted: "endAddressEnc", Kind: KindString,
			ScrubSQL: `''`, HasData: `"endAddress" IS NOT NULL AND "endAddress" <> ''`},
	}},
}

// TargetResult is the per-target outcome of a purge pass.
type TargetResult struct {
	// Scanned is how many rows still held scrubbable plaintext.
	Scanned int
	// Purged is how many rows were verified and scrubbed. Always 0 on a
	// dry run.
	Purged int
	// Redundant counts purged rows whose plaintext exactly equalled its
	// decrypted ciphertext — the pre-deploy shape.
	Redundant int
	// Stale counts purged rows whose plaintext differed from its
	// decrypted ciphertext. Expected and healthy on a live database: the
	// ciphertext moved on while the plaintext stayed frozen. On a busy
	// fleet this should be the MAJORITY.
	Stale int
	// Unsealed counts rows with no ciphertext sibling. Their plaintext is
	// the ONLY copy, so it is left in place — run the matching backfill
	// first.
	Unsealed int
	// Undecryptable counts rows whose ciphertext would not decrypt —
	// typically a key mistake. Left in place.
	Undecryptable int
	// UpdateErrors counts rows that verified but whose scrub UPDATE
	// failed.
	UpdateErrors int
	// Remaining is the post-pass count of rows still holding plaintext.
	// This is the number that must reach zero for the MYR-433 acceptance
	// bar to hold.
	Remaining int
}

// Blocked reports rows left in place because they could not be verified.
// A non-zero total means the purge is incomplete by design and the
// operator has something to fix.
func (r TargetResult) Blocked() int { return r.Unsealed + r.Undecryptable }

// Result aggregates every target's outcome, keyed by Target.Label().
type Result struct {
	Targets map[string]TargetResult
	DryRun  bool
}

func (r Result) sum(pick func(TargetResult) int) int {
	n := 0
	for _, t := range r.Targets {
		n += pick(t)
	}
	return n
}

// TotalPurged sums the scrubbed rows across all targets.
func (r Result) TotalPurged() int { return r.sum(func(t TargetResult) int { return t.Purged }) }

// TotalStale sums the purged-because-stale rows across all targets.
func (r Result) TotalStale() int { return r.sum(func(t TargetResult) int { return t.Stale }) }

// TotalBlocked sums the unverifiable rows across all targets.
func (r Result) TotalBlocked() int { return r.sum(func(t TargetResult) int { return t.Blocked() }) }

// TotalRemaining sums the post-pass residue. The MYR-433 bar is met when
// this is zero.
func (r Result) TotalRemaining() int { return r.sum(func(t TargetResult) int { return t.Remaining }) }

// UpdateErrors sums scrub-write failures across all targets.
func (r Result) UpdateErrors() int { return r.sum(func(t TargetResult) int { return t.UpdateErrors }) }

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

// Run purges every target. When dryRun is true the rows are scanned and
// verified but nothing is written, so an operator can see exactly what
// would happen — and what is blocked — before committing.
//
// Per-row failures never abort the run: one corrupt row must not keep the
// rest of the fleet's credentials sitting in plaintext. They are tallied
// in the Result instead, and the caller decides the exit code.
func (p *Purger) Run(ctx context.Context, dryRun bool) (Result, error) {
	res := Result{Targets: make(map[string]TargetResult, len(Targets)), DryRun: dryRun}

	for _, tgt := range Targets {
		tr, err := p.purgeTarget(ctx, tgt, dryRun)
		if err != nil {
			res.Targets[tgt.Label()] = tr
			return res, err
		}

		remaining, err := countRemaining(ctx, p.pool, tgt)
		if err != nil {
			res.Targets[tgt.Label()] = tr
			return res, err
		}
		tr.Remaining = remaining
		res.Targets[tgt.Label()] = tr

		if p.logger != nil {
			p.logger.Info("plaintextpurge: target complete",
				slog.String("target", tgt.Label()),
				slog.Bool("dry_run", dryRun),
				slog.Int("scanned", tr.Scanned),
				slog.Int("purged", tr.Purged),
				slog.Int("stale", tr.Stale),
				slog.Int("blocked", tr.Blocked()),
				slog.Int("remaining", tr.Remaining),
			)
		}
	}
	return res, nil
}

// candidate is a row that verified and may be scrubbed.
type candidate struct {
	id    string
	stale bool
}

// memberScan is one member's three scanned values for one row, kept
// together rather than as parallel slices so a member can never be
// verified against a different member's ciphertext.
type memberScan struct {
	member     Member
	hasData    bool
	plaintext  *string
	ciphertext *string
}

// purgeTarget runs one target's verify-then-scrub pass.
//
// Plaintext is pulled as ::text so a single scan path serves floats,
// jsonb and text alike; each Kind reinterprets that text during
// verification. Each member also selects its own HasData predicate as a
// boolean so the loop can tell "this half is already scrubbed" from
// "this half is unsealed" without re-deriving the rule in Go.
func (p *Purger) purgeTarget(ctx context.Context, tgt Target, dryRun bool) (TargetResult, error) {
	var tr TargetResult

	cols := make([]string, 0, len(tgt.Members)*3)
	for _, m := range tgt.Members {
		cols = append(cols,
			"("+m.HasData+")",
			fmt.Sprintf("%q::text", m.Plaintext),
			fmt.Sprintf("%q", m.Encrypted),
		)
	}
	selectSQL := fmt.Sprintf(`SELECT "id", %s FROM %q WHERE %s`,
		strings.Join(cols, ", "), tgt.Table, tgt.hasDataPredicate())

	verified, err := p.scanCandidates(ctx, tgt, selectSQL, &tr)
	if err != nil {
		return tr, err
	}

	if dryRun {
		// Attribute the would-be outcomes so a dry run's report is
		// directly comparable to the real run's.
		for _, c := range verified {
			if c.stale {
				tr.Stale++
			} else {
				tr.Redundant++
			}
		}
		return tr, nil
	}

	// Scrub after the cursor is closed rather than inside the iteration:
	// the UPDATE re-checks the ciphertext is still present, and running it
	// against rows a live server may be concurrently writing is safer once
	// we are no longer holding a portal open on the same table.
	scrubSQL := buildScrubSQL(tgt)
	for _, c := range verified {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return tr, fmt.Errorf("plaintextpurge: cancelled: %w", ctxErr)
		}
		if _, uErr := p.pool.Exec(ctx, scrubSQL, c.id); uErr != nil {
			tr.UpdateErrors++
			if p.logger != nil {
				p.logger.Warn("plaintextpurge: scrub failed",
					slog.String("target", tgt.Label()),
					slog.String("id", c.id),
					slog.String("error", uErr.Error()))
			}
			continue
		}
		tr.Purged++
		if c.stale {
			tr.Stale++
		} else {
			tr.Redundant++
		}
	}
	return tr, nil
}

// scanCandidates walks the target's rows, verifies each, and returns the
// ones safe to scrub. Blocked rows are tallied into tr as it goes.
func (p *Purger) scanCandidates(
	ctx context.Context, tgt Target, selectSQL string, tr *TargetResult,
) ([]candidate, error) {
	rows, err := p.pool.Query(ctx, selectSQL)
	if err != nil {
		return nil, fmt.Errorf("plaintextpurge: select %s: %w", tgt.Label(), err)
	}
	defer rows.Close()

	var verified []candidate
	for rows.Next() {
		var id string
		scans := make([]memberScan, len(tgt.Members))

		dests := make([]any, 0, 1+len(tgt.Members)*3)
		dests = append(dests, &id)
		for i := range scans {
			scans[i].member = tgt.Members[i]
			dests = append(dests, &scans[i].hasData, &scans[i].plaintext, &scans[i].ciphertext)
		}
		if scanErr := rows.Scan(dests...); scanErr != nil {
			return nil, fmt.Errorf("plaintextpurge: scan %s row: %w", tgt.Label(), scanErr)
		}
		tr.Scanned++

		v, anyStale := p.verifyTarget(id, scans)
		switch v {
		case verdictScrub:
			verified = append(verified, candidate{id: id, stale: anyStale})
		case verdictUnsealed:
			tr.Unsealed++
		case verdictUndecryptable:
			tr.Undecryptable++
		}
	}
	if iterErr := rows.Err(); iterErr != nil {
		return nil, fmt.Errorf("plaintextpurge: iterate %s rows: %w", tgt.Label(), iterErr)
	}
	return verified, nil
}

// buildScrubSQL emits the all-or-nothing UPDATE for a target: every
// member's plaintext is set to its scrub value in one statement, guarded
// on every member's ciphertext still being present.
func buildScrubSQL(tgt Target) string {
	sets := make([]string, 0, len(tgt.Members))
	guards := make([]string, 0, len(tgt.Members))
	for _, m := range tgt.Members {
		sets = append(sets, fmt.Sprintf("%q = %s", m.Plaintext, m.ScrubSQL))
		guards = append(guards, fmt.Sprintf("%q IS NOT NULL", m.Encrypted))
	}
	return fmt.Sprintf(`UPDATE %q SET %s WHERE "id" = $1 AND %s`,
		tgt.Table, strings.Join(sets, ", "), strings.Join(guards, " AND "))
}

// countRemaining reports how many rows still hold plaintext for this
// target — the operator-facing "is this done?" number.
func countRemaining(ctx context.Context, p pool, tgt Target) (int, error) {
	sql := fmt.Sprintf(`SELECT COUNT(*) FROM %q WHERE %s`, tgt.Table, tgt.hasDataPredicate())
	var n int
	if err := p.QueryRow(ctx, sql).Scan(&n); err != nil {
		return 0, fmt.Errorf("plaintextpurge: count %s: %w", tgt.Label(), err)
	}
	return n, nil
}

// CountRemaining reports the residual plaintext row count for every
// target, keyed by Target.Label(). Exported so verification tests and
// operator tooling can assert the MYR-433 bar without owning a Purger.
func CountRemaining(ctx context.Context, p pool) (map[string]int, error) {
	out := make(map[string]int, len(Targets))
	for _, tgt := range Targets {
		n, err := countRemaining(ctx, p, tgt)
		if err != nil {
			return nil, err
		}
		out[tgt.Label()] = n
	}
	return out, nil
}
