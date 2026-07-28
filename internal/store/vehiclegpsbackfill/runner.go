// Backfill row scanner and per-row UPDATE issuer. Split out of
// backfill.go so neither file approaches the 300-line cap.

package vehiclegpsbackfill

import (
	"context"
	"fmt"
	"log/slog"
)

// pendingPair holds the encrypted pair output destined for one row.
// Both halves are populated together (atomic-pair invariant) or both
// are nil.
type pendingPair struct {
	latEnc *string
	lngEnc *string
}

// pendingRow is one row's pre-update state. Only the pairs that need
// encryption are set; the others are zero-value (lat/lng nil) so the
// UPDATE COALESCE skips them.
type pendingRow struct {
	id       string
	main     pendingPair
	dest     pendingPair
	origin   pendingPair
}

// hasWork reports whether the row needs an UPDATE.
func (p pendingRow) hasWork() bool {
	return p.main.latEnc != nil || p.dest.latEnc != nil || p.origin.latEnc != nil
}

// collectBatch executes the SELECT and converts each scanned row into
// a pendingRow. A nil returned slice means the SELECT/scan itself
// failed (fatal); a non-nil slice with a non-nil error means soft
// encryption failures during the scan loop (recoverable; some rows
// still ready to UPDATE).
func (b *Backfiller) collectBatch(ctx context.Context, res *Result) ([]pendingRow, error) {
	rows, err := b.pool.Query(ctx, selectSQL)
	if err != nil {
		return nil, fmt.Errorf("vehiclegpsbackfill: select rows: %w", err)
	}
	defer rows.Close()

	batch := make([]pendingRow, 0)
	var firstErr error
	for rows.Next() {
		var (
			id                                 string
			latPT, lngPT                       *float64
			latEnc, lngEnc                     *string
			destLatPT, destLngPT               *float64
			destLatEnc, destLngEnc             *string
			originLatPT, originLngPT           *float64
			originLatEnc, originLngEnc         *string
		)
		if err := rows.Scan(&id,
			&latPT, &lngPT, &latEnc, &lngEnc,
			&destLatPT, &destLngPT, &destLatEnc, &destLngEnc,
			&originLatPT, &originLngPT, &originLatEnc, &originLngEnc,
		); err != nil {
			return nil, fmt.Errorf("vehiclegpsbackfill: scan row: %w", err)
		}
		res.RowsScanned++
		row, pErr := b.encryptRow(id,
			latPT, lngPT, latEnc, lngEnc,
			destLatPT, destLngPT, destLatEnc, destLngEnc,
			originLatPT, originLngPT, originLatEnc, originLngEnc,
			res,
		)
		if pErr != nil && firstErr == nil {
			firstErr = pErr
		}
		if row.hasWork() {
			batch = append(batch, row)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vehiclegpsbackfill: iterate rows: %w", err)
	}
	return batch, firstErr
}

// encryptRow encrypts every (lat, lng) pair on the row that needs
// encryption. The atomic-pair invariant: a pair is encrypted only when
// BOTH plaintext halves are present and at least one ciphertext half
// is missing. Half-pair plaintext (one nil) is left untouched.
func (b *Backfiller) encryptRow(
	id string,
	latPT, lngPT *float64, latEnc, lngEnc *string,
	destLatPT, destLngPT *float64, destLatEnc, destLngEnc *string,
	originLatPT, originLngPT *float64, originLatEnc, originLngEnc *string,
	res *Result,
) (pendingRow, error) {
	row := pendingRow{id: id}
	var firstErr error
	encryptOne := func(latPT, lngPT *float64, latEnc, lngEnc *string, label string) (pendingPair, error) {
		if !needsEncrypt(latPT, lngPT, latEnc, lngEnc) {
			return pendingPair{}, nil
		}
		latCT, err := b.encryptor.EncryptString(formatFloat(*latPT))
		if err != nil {
			return pendingPair{}, fmt.Errorf("encrypt %s.lat (id=%s): %w", label, id, err)
		}
		lngCT, err := b.encryptor.EncryptString(formatFloat(*lngPT))
		if err != nil {
			return pendingPair{}, fmt.Errorf("encrypt %s.lng (id=%s): %w", label, id, err)
		}
		return pendingPair{latEnc: &latCT, lngEnc: &lngCT}, nil
	}
	for _, p := range []struct {
		latPT, lngPT *float64
		latEnc, lngEnc *string
		dst   *pendingPair
		label string
	}{
		{latPT, lngPT, latEnc, lngEnc, &row.main, "main"},
		{destLatPT, destLngPT, destLatEnc, destLngEnc, &row.dest, "dest"},
		{originLatPT, originLngPT, originLatEnc, originLngEnc, &row.origin, "origin"},
	} {
		pair, err := encryptOne(p.latPT, p.lngPT, p.latEnc, p.lngEnc, p.label)
		if err != nil {
			res.EncryptErrors++
			if firstErr == nil {
				firstErr = err
			}
			if b.logger != nil {
				b.logger.Warn("vehiclegpsbackfill: encrypt failed", slog.String("error", err.Error()))
			}
			continue
		}
		if pair.latEnc != nil {
			*p.dst = pair
			res.PairsEncrypted++
		}
	}
	return row, firstErr
}

// needsEncrypt is the atomic-pair gate: encrypt only if both plaintext
// halves are present and at least one ciphertext half is missing.
// Half-pair plaintext (one nil) is skipped — there's no atomic-pair
// answer for "encrypt half a pair".
func needsEncrypt(latPT, lngPT *float64, latEnc, lngEnc *string) bool {
	if latPT == nil || lngPT == nil {
		return false
	}
	latEncPresent := latEnc != nil && *latEnc != ""
	lngEncPresent := lngEnc != nil && *lngEnc != ""
	if latEncPresent && lngEncPresent {
		return false
	}
	return true
}

// applyBatch issues the per-row UPDATE for every pending entry.
// Returns the first UPDATE error (if any); subsequent failures are
// tallied in res.UpdateErrors but don't short-circuit the loop.
func (b *Backfiller) applyBatch(ctx context.Context, batch []pendingRow, res *Result) error {
	var firstErr error
	for _, row := range batch {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("vehiclegpsbackfill: cancelled: %w", ctxErr)
		}
		tag, uErr := b.pool.Exec(ctx, updateSQL,
			row.id,
			row.main.latEnc, row.main.lngEnc,
			row.dest.latEnc, row.dest.lngEnc,
			row.origin.latEnc, row.origin.lngEnc,
		)
		if uErr != nil {
			res.UpdateErrors++
			if firstErr == nil {
				firstErr = fmt.Errorf("update row id=%s: %w", row.id, uErr)
			}
			if b.logger != nil {
				b.logger.Warn("vehiclegpsbackfill: update failed", slog.String("id", row.id), slog.String("error", uErr.Error()))
			}
			continue
		}
		if tag.RowsAffected() > 0 {
			res.RowsUpdated++
		}
	}
	return firstErr
}

// selectSQL pulls every row that has at least one plaintext-without-
// ciphertext GPS half. The OR'd predicates cover all six columns. The
// SELECT pulls plaintext + ciphertext halves for every pair so
// encryptRow can apply the atomic-pair guard without a second query.
const selectSQL = `SELECT "id",
    "latitude", "longitude", "latitudeEnc", "longitudeEnc",
    "destinationLatitude", "destinationLongitude",
    "destinationLatitudeEnc", "destinationLongitudeEnc",
    "originLatitude", "originLongitude",
    "originLatitudeEnc", "originLongitudeEnc"
FROM "Vehicle"
WHERE
    ("latitude" IS NOT NULL AND "latitudeEnc" IS NULL)
 OR ("longitude" IS NOT NULL AND "longitudeEnc" IS NULL)
 OR ("destinationLatitude" IS NOT NULL AND "destinationLatitudeEnc" IS NULL)
 OR ("destinationLongitude" IS NOT NULL AND "destinationLongitudeEnc" IS NULL)
 OR ("originLatitude" IS NOT NULL AND "originLatitudeEnc" IS NULL)
 OR ("originLongitude" IS NOT NULL AND "originLongitudeEnc" IS NULL)`

// updateSQL writes whichever ciphertext halves the caller passes;
// COALESCE leaves already-encrypted pairs alone so a partial mid-run
// state is recoverable on a re-run.
const updateSQL = `UPDATE "Vehicle"
SET "latitudeEnc"             = COALESCE($2, "latitudeEnc"),
    "longitudeEnc"            = COALESCE($3, "longitudeEnc"),
    "destinationLatitudeEnc"  = COALESCE($4, "destinationLatitudeEnc"),
    "destinationLongitudeEnc" = COALESCE($5, "destinationLongitudeEnc"),
    "originLatitudeEnc"       = COALESCE($6, "originLatitudeEnc"),
    "originLongitudeEnc"      = COALESCE($7, "originLongitudeEnc")
WHERE "id" = $1`
