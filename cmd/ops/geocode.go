package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/myrobotaxi/telemetry/internal/geocode"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// geocodeBackfillTimeout is the per-request HTTP timeout passed to the
// Mapbox geocoder. Mirrors the server's default drives.geocode_timeout
// (5s, internal/config/defaults.go) — the backfill hits the exact same
// API, just from an operator's machine/CI runner instead of inline on
// drive.started/drive.ended.
const geocodeBackfillTimeout = 5 * time.Second

func runGeocode(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("geocode requires a subcommand (backfill)")
	}
	switch args[0] {
	case "backfill":
		return runGeocodeBackfill(ctx, args[1:])
	default:
		return fmt.Errorf("unknown geocode subcommand %q", args[0])
	}
}

// runGeocodeBackfill implements `ops geocode backfill` (MYR-240): finds
// every Drive row still missing a startAddress or endAddress — the
// lasting damage from the `limit`+multi-type 422 that broke every
// production reverse geocode until the request-shape fix — and
// re-geocodes it from the row's own recorded route points (Drive has no
// dedicated start/end lat/lng columns). Respects the geocoder's
// rate limiter (the default MapboxGeocoder construction below: 10 req/s,
// burst 10, same as the running server) and honors --dry-run.
func runGeocodeBackfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("geocode backfill", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "log what would change without writing to the database")
	limit := fs.Int("limit", 0, "stop after this many rows are processed (0 = no limit)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := newLogger()

	token := os.Getenv("MAPBOX_TOKEN")
	geocoder := geocode.NewMapboxGeocoder(token, geocodeBackfillTimeout)
	if geocoder == nil {
		return fmt.Errorf("MAPBOX_TOKEN is required")
	}

	db, err := openDB(ctx, logger)
	if err != nil {
		return err
	}
	defer db.Close()

	drives, err := newDriveRepoForBackfill(db, logger)
	if err != nil {
		return err
	}

	rows, err := drives.ListMissingAddresses(ctx)
	if err != nil {
		return fmt.Errorf("list rows missing addresses: %w", err)
	}
	logger.Info("geocode backfill: starting",
		slog.Int("rows_found", len(rows)),
		slog.Bool("dry_run", *dryRun),
		slog.Int("limit", *limit),
	)

	var processed, updated, skipped, failed int
	for _, row := range rows {
		if *limit > 0 && processed >= *limit {
			logger.Info("geocode backfill: reached --limit, stopping", slog.Int("limit", *limit))
			break
		}
		processed++

		switch backfillRow(ctx, geocoder, drives, row, *dryRun, logger) {
		case backfillUpdated:
			updated++
		case backfillSkipped:
			skipped++
		case backfillFailed:
			failed++
		}
	}

	logger.Info("geocode backfill: done",
		slog.Int("rows_found", len(rows)),
		slog.Int("processed", processed),
		slog.Int("updated", updated),
		slog.Int("skipped", skipped),
		slog.Int("failed", failed),
		slog.Bool("dry_run", *dryRun),
	)
	if failed > 0 {
		return fmt.Errorf("geocode backfill: %d row(s) failed to update", failed)
	}
	return nil
}

// newDriveRepoForBackfill builds the DriveRepo the geocode backfill
// reads through. A usable ENCRYPTION_KEY is REQUIRED.
//
// This used to degrade to a plaintext-only DriveRepo on a missing or
// broken key, on the reasoning that "the plaintext routePoints column is
// the source of truth regardless of whether the encrypted shadow
// exists". MYR-433 inverted that: routePointsEnc is now the only store
// for a drive's GPS trail. A keyless run would read every trail as
// empty, geocode nothing, and report success — the worst possible
// outcome for a backfill. So the key is a hard requirement and its
// absence is an error the operator sees immediately.
func newDriveRepoForBackfill(db *store.DB, logger *slog.Logger) (*store.DriveRepo, error) {
	repo, err := newDriveRepo(db, logger)
	if err != nil {
		return nil, fmt.Errorf("geocode backfill requires a usable ENCRYPTION_KEY "+
			"(drive GPS trails are stored encrypted since MYR-433): %w", err)
	}
	return repo, nil
}

// backfillOutcome classifies what happened to one Drive row so the
// caller can tally a per-run summary.
type backfillOutcome int

const (
	backfillSkipped backfillOutcome = iota
	backfillUpdated
	backfillFailed
)

// driveAddressUpdater is the narrow slice of *store.DriveRepo backfillRow
// needs. Defined at the consumer per the repo's interfaces-at-consumer-
// site convention, and narrow enough that a fake can exercise the
// ErrDriveNotFound-as-skip race-handling branch below without a real
// database.
type driveAddressUpdater interface {
	UpdateAddresses(ctx context.Context, id string, startLocation, startAddress, endLocation, endAddress *string) error
}

// backfillRow reverse-geocodes whichever side(s) of one Drive row are
// missing an address, using the first (start) and last (end) point
// recorded in routePoints. Logs a per-row outcome unconditionally — a
// lookup miss (geocode.ErrNoResult), an invalid/zero-GPS coordinate, or
// no usable route points at all is a soft skip, matching how the
// writer's inline geocode calls already treat these cases (see
// internal/store/writer_drives.go).
//
// The end side is skipped whenever row.EndTime is empty, i.e. the drive
// is still open — belt-and-braces alongside queryDriveMissingAddresses'
// own endTime guard. Every Drive row is created with both endTime and
// endAddress set to the empty string (mapDriveStarted); without this
// guard a backfill run against a still-driving vehicle would
// reverse-geocode the LAST routePoints entry — the car's current,
// still-changing position — and persist it as the drive's permanent
// endLocation/endAddress.
func backfillRow(ctx context.Context, geocoder geocode.Geocoder, drives driveAddressUpdater, row store.DriveBackfillRow, dryRun bool, logger *slog.Logger) backfillOutcome {
	startPt, endPt, ok := decodeRouteEndpoints(row.RoutePoints)
	if !ok {
		logger.Warn("geocode backfill: skip row, no usable route points", slog.String("drive_id", row.ID))
		return backfillSkipped
	}

	var startLocation, startAddress *string
	if row.StartAddress == "" {
		startLocation, startAddress = geocodeSide(ctx, geocoder, row.ID, "start", startPt, logger)
	}
	var endLocation, endAddress *string
	if row.EndAddress == "" && row.EndTime != "" {
		endLocation, endAddress = geocodeSide(ctx, geocoder, row.ID, "end", endPt, logger)
	}

	if startAddress == nil && endAddress == nil {
		logger.Info("geocode backfill: skip row, nothing to write this run", slog.String("drive_id", row.ID))
		return backfillSkipped
	}

	// startAddress/endAddress are P1 ("Log-safe: No" per
	// docs/contracts/data-classification.md §1.4) — logSafePresence
	// summarizes whether a value was resolved without exposing the
	// place name / street address itself.
	if dryRun {
		logger.Info("geocode backfill: dry-run, would update",
			slog.String("drive_id", row.ID),
			slog.String("start_address", logSafePresence(startAddress)),
			slog.String("end_address", logSafePresence(endAddress)),
		)
		return backfillUpdated
	}

	if err := drives.UpdateAddresses(ctx, row.ID, startLocation, startAddress, endLocation, endAddress); err != nil {
		// The row can vanish between this backfill's SELECT and this
		// UPDATE — handleDriveDiscarded hard-deletes micro-drives the
		// detector later decides were too short. That's a benign race,
		// not a backfill failure.
		if errors.Is(err, store.ErrDriveNotFound) {
			logger.Warn("geocode backfill: skip row, deleted before update (likely a discarded micro-drive)",
				slog.String("drive_id", row.ID),
			)
			return backfillSkipped
		}
		// Unlike geocodeSide's errors, DriveRepo.UpdateAddresses errors
		// wrap a plain DB/pgx failure keyed by drive_id — no GPS
		// coordinates or place-name text ever enters this error string,
		// so logging it verbatim doesn't trip the P1 log-safety rule.
		logger.Error("geocode backfill: update failed",
			slog.String("drive_id", row.ID),
			slog.String("error", err.Error()),
		)
		return backfillFailed
	}
	logger.Info("geocode backfill: updated",
		slog.String("drive_id", row.ID),
		slog.String("start_address", logSafePresence(startAddress)),
		slog.String("end_address", logSafePresence(endAddress)),
	)
	return backfillUpdated
}

// logSafePresence summarizes a nilable P1 string for logging without
// exposing its value. Drive.startAddress/endAddress and the
// reverse-geocoded place name are P1, "Log-safe: No" per
// docs/contracts/data-classification.md §1.4 — presence + length is
// enough to verify a dry-run or confirm a write landed without putting
// a street address or place name into structured logs.
func logSafePresence(s *string) string {
	if s == nil {
		return "-"
	}
	if *s == "" {
		return "empty"
	}
	return fmt.Sprintf("set(len=%d)", len(*s))
}

// geocodeSide reverse-geocodes one endpoint and returns pointers to the
// place name / address to write, or (nil, nil) when nothing should be
// written for this side — a zero-GPS point (Tesla's "no fix" sentinel,
// same check as writer_drives.go), an out-of-range coordinate
// (geocode.ErrInvalidCoordinate), or a soft ErrNoResult miss. All three
// are logged and treated as non-fatal to the row.
func geocodeSide(ctx context.Context, geocoder geocode.Geocoder, driveID, side string, pt store.RoutePointRecord, logger *slog.Logger) (location, address *string) {
	if pt.Latitude == 0 && pt.Longitude == 0 {
		logger.Warn("geocode backfill: skip side, zero-GPS coordinate",
			slog.String("drive_id", driveID), slog.String("side", side),
		)
		return nil, nil
	}

	result, err := geocoder.ReverseGeocode(ctx, pt.Latitude, pt.Longitude)
	switch {
	case err == nil:
		loc, addr := result.PlaceName, result.Address
		return &loc, &addr
	case errors.Is(err, geocode.ErrNoResult):
		logger.Warn("geocode backfill: no result for coordinate",
			slog.String("drive_id", driveID), slog.String("side", side),
		)
		return nil, nil
	default:
		// MYR-254: geocode.ReverseGeocode's errors are sanitized at the
		// source (internal/geocode/geocode.go) — none of them ever embed
		// the queried coordinates, the request URL/access_token, or the
		// Mapbox response body, so err.Error() itself is safe to log.
		// Still prefer geocode.ClassifyError's coarse tag over the raw
		// string: it's a stable, greppable value across Mapbox error
		// message changes, and it's a defense-in-depth backstop against a
		// future geocode.go change accidentally reintroducing sensitive
		// detail into an error string.
		logger.Warn("geocode backfill: reverse geocode failed",
			slog.String("drive_id", driveID), slog.String("side", side),
			slog.String("error_class", geocode.ClassifyError(err)),
		)
		return nil, nil
	}
}

// decodeRouteEndpoints unmarshals a Drive.routePoints JSONB array and
// returns its first and last recorded points. ok is false when the
// array is empty or malformed — the row has no coordinates to backfill
// from at all, regardless of which side is missing an address.
func decodeRouteEndpoints(raw json.RawMessage) (start, end store.RoutePointRecord, ok bool) {
	if len(raw) == 0 {
		return store.RoutePointRecord{}, store.RoutePointRecord{}, false
	}
	var pts []store.RoutePointRecord
	if err := json.Unmarshal(raw, &pts); err != nil || len(pts) == 0 {
		return store.RoutePointRecord{}, store.RoutePointRecord{}, false
	}
	return pts[0], pts[len(pts)-1], true
}
