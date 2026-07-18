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

	"github.com/myrobotaxi/telemetry/internal/cryptox"
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

	drives := newDriveRepoForBackfill(db, logger)

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

// newDriveRepoForBackfill prefers the encrypted routePointsEnc read path
// when ENCRYPTION_KEY (or the versioned shape) is configured, but falls
// back to the legacy plaintext-only DriveRepo when it isn't — unlike the
// running server, this ops tool must not hard-fail on a missing key: the
// dual-write's plaintext routePoints column is the source of truth
// regardless of whether the encrypted shadow exists (see drive_repo.go),
// so a plaintext-only read still returns correct coordinates for every
// row that has any.
func newDriveRepoForBackfill(db *store.DB, logger *slog.Logger) *store.DriveRepo {
	ks, err := cryptox.LoadKeySetFromEnv()
	if err != nil {
		logger.Warn("geocode backfill: no usable ENCRYPTION_KEY, reading routePoints plaintext-only",
			slog.String("detail", err.Error()),
		)
		return store.NewDriveRepo(db.Pool(), store.NoopMetrics{})
	}
	enc, err := cryptox.NewEncryptor(ks)
	if err != nil {
		logger.Warn("geocode backfill: ENCRYPTION_KEY set but unusable, reading routePoints plaintext-only",
			slog.String("detail", err.Error()),
		)
		return store.NewDriveRepo(db.Pool(), store.NoopMetrics{})
	}
	return store.NewDriveRepoWithEncryption(db.Pool(), store.NoopMetrics{}, enc, logger)
}

// backfillOutcome classifies what happened to one Drive row so the
// caller can tally a per-run summary.
type backfillOutcome int

const (
	backfillSkipped backfillOutcome = iota
	backfillUpdated
	backfillFailed
)

// backfillRow reverse-geocodes whichever side(s) of one Drive row are
// missing an address, using the first (start) and last (end) point
// recorded in routePoints. Logs a per-row outcome unconditionally — a
// lookup miss (geocode.ErrNoResult), an invalid/zero-GPS coordinate, or
// no usable route points at all is a soft skip, matching how the
// writer's inline geocode calls already treat these cases (see
// internal/store/writer_drives.go).
func backfillRow(ctx context.Context, geocoder geocode.Geocoder, drives *store.DriveRepo, row store.DriveBackfillRow, dryRun bool, logger *slog.Logger) backfillOutcome {
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
	if row.EndAddress == "" {
		endLocation, endAddress = geocodeSide(ctx, geocoder, row.ID, "end", endPt, logger)
	}

	if startAddress == nil && endAddress == nil {
		logger.Info("geocode backfill: skip row, nothing to write this run", slog.String("drive_id", row.ID))
		return backfillSkipped
	}

	if dryRun {
		logger.Info("geocode backfill: dry-run, would update",
			slog.String("drive_id", row.ID),
			slog.Any("start_address", startAddress),
			slog.Any("end_address", endAddress),
		)
		return backfillUpdated
	}

	if err := drives.UpdateAddresses(ctx, row.ID, startLocation, startAddress, endLocation, endAddress); err != nil {
		logger.Error("geocode backfill: update failed",
			slog.String("drive_id", row.ID),
			slog.String("error", err.Error()),
		)
		return backfillFailed
	}
	logger.Info("geocode backfill: updated",
		slog.String("drive_id", row.ID),
		slog.Any("start_address", startAddress),
		slog.Any("end_address", endAddress),
	)
	return backfillUpdated
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
		logger.Warn("geocode backfill: reverse geocode failed",
			slog.String("drive_id", driveID), slog.String("side", side), slog.String("error", err.Error()),
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
