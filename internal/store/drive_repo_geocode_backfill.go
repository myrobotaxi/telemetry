// MYR-240 backfill read/write path. Split out of drive_repo.go (already
// at the 300-line file cap) so the "find Drive rows missing an address,
// surface their routePoints for coordinate extraction, write the
// geocoded result back" flow used by `ops geocode backfill` has its own
// home, adjacent to the wide-read / dual-write module it borrows the
// routePointsEnc decrypt preference from.
//
// The Drive table has no dedicated start/end latitude/longitude columns
// (see queries.go queryDriveMissingAddresses) — the only source of
// coordinates to re-geocode is the first and last point recorded in
// routePoints, which is why this file returns the full array rather than
// a pre-extracted pair; the caller (cmd/ops) owns the "first/last point,
// skip zero-GPS" decision so store stays a thin persistence layer.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// DriveBackfillRow is the projection ListMissingAddresses returns for
// the MYR-240 backfill sweep: the row's current (empty) address columns,
// so the caller knows which side(s) still need geocoding, plus the
// resolved routePoints array to pull coordinates from.
//
// EndTime is surfaced (not just used server-side in
// queryDriveMissingAddresses' WHERE clause) as a belt-and-braces guard:
// the caller MUST skip the end side entirely for a row whose EndTime is
// empty (drive still open) even though the query already excludes those
// rows from matching on an empty endAddress — see queries.go for why a
// still-open drive's routePoints last entry is the car's current,
// still-changing position and must never be written as endAddress.
type DriveBackfillRow struct {
	ID           string
	StartAddress string
	EndAddress   string
	EndTime      string
	RoutePoints  json.RawMessage
}

// ListMissingAddresses returns every Drive row whose startAddress is
// empty, or whose endAddress is empty on a row that has actually ended,
// oldest first. Used exclusively by `ops geocode backfill` (MYR-240) —
// the running server always attempts a reverse geocode inline on
// drive.started/drive.ended, so in steady state this result set only
// holds rows from before the request-shape fix landed (every reverse
// geocode 422'd) or rows where the geocoder had no result for that
// coordinate.
//
// RoutePoints is decrypted from routePointsEnc the same way GetByID does
// it — reusing applyResolvedRoutePoints via a throwaway DriveRecord so
// the two read paths can't drift.
//
// MYR-433: rows whose trail cannot be decrypted yield an empty
// RoutePoints and are skipped by the caller rather than geocoded from a
// plaintext column. A backfill run without a usable ENCRYPTION_KEY
// therefore does nothing at all, which is why cmd/ops/geocode.go now
// requires the key up front instead of silently degrading.
func (r *DriveRepo) ListMissingAddresses(ctx context.Context) ([]DriveBackfillRow, error) {
	start := time.Now()
	rows, err := r.pool.Query(ctx, queryDriveMissingAddresses)
	if err != nil {
		r.metrics.IncQueryError("drive.list_missing_addresses")
		r.metrics.ObserveQueryDuration("drive.list_missing_addresses", time.Since(start).Seconds())
		return nil, fmt.Errorf("DriveRepo.ListMissingAddresses: %w", err)
	}
	defer rows.Close()

	var out []DriveBackfillRow
	for rows.Next() {
		var d DriveBackfillRow
		var routePointsEnc *string
		var startAddrEnc, endAddrEnc *string
		if scanErr := rows.Scan(&d.ID, &startAddrEnc, &endAddrEnc, &d.EndTime, &routePointsEnc); scanErr != nil {
			r.metrics.IncQueryError("drive.list_missing_addresses")
			r.metrics.ObserveQueryDuration("drive.list_missing_addresses", time.Since(start).Seconds())
			return nil, fmt.Errorf("DriveRepo.ListMissingAddresses: scan: %w", scanErr)
		}
		// MYR-447: the addresses come back sealed. Decrypting them here
		// keeps DriveBackfillRow's two plain-string fields — and therefore
		// cmd/ops' `row.StartAddress == ""` side selection — working
		// exactly as before.
		d.StartAddress = r.openDriveLabel(startAddrEnc, "startAddressEnc")
		d.EndAddress = r.openDriveLabel(endAddrEnc, "endAddressEnc")
		rec := DriveRecord{ID: d.ID}
		r.applyResolvedRoutePoints(&rec, routePointsEnc)
		d.RoutePoints = rec.RoutePoints
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		r.metrics.IncQueryError("drive.list_missing_addresses")
		r.metrics.ObserveQueryDuration("drive.list_missing_addresses", time.Since(start).Seconds())
		return nil, fmt.Errorf("DriveRepo.ListMissingAddresses: rows: %w", err)
	}
	r.metrics.ObserveQueryDuration("drive.list_missing_addresses", time.Since(start).Seconds())
	return out, nil
}

// UpdateAddresses writes the geocoded start/end location+address values
// for one Drive row (MYR-240 backfill). Each argument is nilable: pass
// nil for a column the caller didn't (re)geocode this run — e.g. only
// the start side was empty, or a lookup returned geocode.ErrNoResult —
// so the existing value (or a value written concurrently by the live
// server between this backfill's SELECT and UPDATE) is preserved rather
// than overwritten with an empty string. Returns ErrDriveNotFound if the
// row no longer exists.
//
// MYR-447: each supplied value is sealed before it reaches the UPDATE and
// lands in the matching `*Enc` column. A supplied-but-EMPTY label seals to
// the absent sentinel and is therefore passed as nil, i.e. treated the
// same as "not geocoded this run" — writing an empty ciphertext would
// make the column non-NULL and permanently hide the row from the
// `IS NULL` discovery predicate without recording anything.
//
// Requires an Encryptor: a keyless repo returns ErrEncryptionRequired
// rather than silently writing NULLs over a row it was asked to fill in.
func (r *DriveRepo) UpdateAddresses(ctx context.Context, id string, startLocation, startAddress, endLocation, endAddress *string) error {
	if r.encryptor == nil {
		return fmt.Errorf("DriveRepo.UpdateAddresses(%s): %w", id, ErrEncryptionRequired)
	}
	sealed, err := r.sealOptionalLabels(startLocation, startAddress, endLocation, endAddress)
	if err != nil {
		return fmt.Errorf("DriveRepo.UpdateAddresses(%s): %w", id, err)
	}

	start := time.Now()
	tag, err := r.pool.Exec(ctx, queryDriveUpdateAddresses, id,
		sealed[0], sealed[1], sealed[2], sealed[3])
	r.metrics.ObserveQueryDuration("drive.update_addresses", time.Since(start).Seconds())
	if err != nil {
		r.metrics.IncQueryError("drive.update_addresses")
		return fmt.Errorf("DriveRepo.UpdateAddresses(%s): %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("DriveRepo.UpdateAddresses(%s): %w", id, ErrDriveNotFound)
	}
	return nil
}
