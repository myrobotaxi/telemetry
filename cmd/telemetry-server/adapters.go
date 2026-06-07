package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/myrobotaxi/telemetry/internal/drives"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// debugFieldsMinTokenLen is the minimum length required for
// DEBUG_FIELDS_TOKEN when the endpoint is enabled in non-dev mode.
// 32 chars ≈ 192 bits of entropy for a base64-encoded random token;
// anything shorter is likely a typo or a test fixture and should be
// rejected at startup rather than shipped to prod.
const debugFieldsMinTokenLen = 32

// debugFieldsGate describes how /api/debug/fields should be mounted at
// startup. It is derived from the --dev flag and the DEBUG_FIELDS_TOKEN
// env var by resolveDebugFieldsGate.
type debugFieldsGate struct {
	// Enabled is true when the endpoint should be mounted.
	Enabled bool
	// Token is the shared secret the client must present; empty means
	// auth is skipped (only valid under --dev).
	Token string
	// Reason is a short human string ("dev mode" or "token set") used
	// in the startup log to tell operators which gate is active.
	Reason string
}

// errDebugFieldsTokenTooShort is returned when a non-dev operator sets
// DEBUG_FIELDS_TOKEN to a value shorter than debugFieldsMinTokenLen.
var errDebugFieldsTokenTooShort = errors.New("DEBUG_FIELDS_TOKEN must be at least 32 chars in non-dev mode")

// resolveDebugFieldsGate folds the --dev flag and DEBUG_FIELDS_TOKEN
// value into a single decision about whether to mount the debug-fields
// endpoint and how to authenticate clients. Rules:
//
//   - --dev:              endpoint mounted, token optional
//   - no --dev + no token: endpoint NOT mounted
//   - no --dev + token:   endpoint mounted, token required; fails if token < 32 chars
func resolveDebugFieldsGate(devMode bool, token string) (debugFieldsGate, error) {
	switch {
	case devMode && token != "":
		return debugFieldsGate{Enabled: true, Token: token, Reason: "dev mode + DEBUG_FIELDS_TOKEN"}, nil
	case devMode:
		return debugFieldsGate{Enabled: true, Reason: "dev mode (no token required)"}, nil
	case token == "":
		return debugFieldsGate{}, nil
	case len(token) < debugFieldsMinTokenLen:
		return debugFieldsGate{}, fmt.Errorf("%w (got %d chars)", errDebugFieldsTokenTooShort, len(token))
	default:
		return debugFieldsGate{Enabled: true, Token: token, Reason: "DEBUG_FIELDS_TOKEN set"}, nil
	}
}

// devLocalhostOriginPatterns is the localhost-friendly fallback used when
// --dev is set and the operator has not configured an explicit origin
// allow-list. Hostname-only patterns match any scheme (http/ws), and the
// "localhost:*" / "127.0.0.1:*" forms admit any port (Next.js typically
// dials from :3000 or :3001 during local development).
var devLocalhostOriginPatterns = []string{
	"localhost",
	"localhost:*",
	"127.0.0.1",
	"127.0.0.1:*",
	"[::1]",
	"[::1]:*",
}

// resolveWSOriginPatterns folds the operator-configured allow-list and
// the --dev flag into the final OriginPatterns slice passed to the
// coder/websocket Accept call. Rules per NFR-3.22 / MYR-17:
//
//   - configured AllowedOrigins takes precedence in every mode.
//   - empty config + --dev: localhost defaults so a dev workstation
//     can dial without ceremony (still fails-closed for any other
//     cross-origin host).
//   - empty config + production: empty slice. coder/websocket's
//     authenticateOrigin permits same-origin and empty-Origin requests
//     and rejects all cross-origin dials with HTTP 403. A loud Warn
//     reminds the operator to set websocket.allowed_origins or
//     WEBSOCKET_ALLOWED_ORIGINS rather than rely on this fallback.
func resolveWSOriginPatterns(configured []string, devMode bool, logger *slog.Logger) []string {
	if len(configured) > 0 {
		logger.Info("websocket allowed origins configured",
			slog.Int("count", len(configured)),
			slog.Any("patterns", configured),
		)
		return configured
	}
	if devMode {
		logger.Warn("websocket allowed origins not configured; --dev fallback admits localhost only",
			slog.Any("patterns", devLocalhostOriginPatterns),
		)
		// Return a copy so callers cannot mutate the package-level slice.
		out := make([]string, len(devLocalhostOriginPatterns))
		copy(out, devLocalhostOriginPatterns)
		return out
	}
	logger.Warn("websocket allowed origins not configured in production mode — all cross-origin connections will be rejected with HTTP 403; set websocket.allowed_origins or WEBSOCKET_ALLOWED_ORIGINS to admit a browser app")
	return nil
}

// vinResolverAdapter adapts store.VINCache to the ws.VINResolver interface
// (returns vehicleID string). Backing the WS broadcaster path with the cache
// avoids fetching the full Vehicle row (including the heavy navRouteCoordinates
// JSON) on every telemetry frame — the slim two-column query runs once per VIN
// for the lifetime of the process.
type vinResolverAdapter struct {
	cache *store.VINCache
}

func (a *vinResolverAdapter) GetByVIN(ctx context.Context, vin string) (string, error) {
	id, err := a.cache.ResolveID(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve VIN: %w", err)
	}
	return id, nil
}

// vehicleOwnerAdapter adapts store.VINCache to the
// telemetry.VehicleOwnerLookup interface (returns owning user ID). Shares
// the same cache instance as vinResolverAdapter, so a single DB lookup per
// VIN serves both ID and owner resolution for the lifetime of the process.
type vehicleOwnerAdapter struct {
	cache *store.VINCache
}

func (a *vehicleOwnerAdapter) GetVehicleOwner(ctx context.Context, vin string) (string, error) {
	userID, err := a.cache.ResolveOwner(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve vehicle owner: %w", err)
	}
	return userID, nil
}

// vehicleListerAdapter adapts store.VehicleRepo.ListSummariesByUser
// to the narrow telemetry.VehicleLister interface used by the
// GET /api/vehicles handler (MYR-91). The adapter exists so the
// handler can stay decoupled from internal/store (which would
// otherwise form an import cycle through cmd/ops).
//
// MYR-122: the adapter binds to the LEAN read path
// (`ListSummariesByUser`) — the catalog list endpoint only emits ~10
// fields per row, so pulling the wide read (37 columns + GPS dual-read
// + nav-route blob decryption) on every call was what made
// `GET /api/vehicles` cost ~1.3 min on a warm DB. Detail/edit
// consumers continue to use the wide `ListByUser` / `GetByID` paths.
type vehicleListerAdapter struct {
	repo *store.VehicleRepo
}

func (a *vehicleListerAdapter) ListByUser(ctx context.Context, userID string) ([]telemetry.VehicleCatalogRow, error) {
	rows, err := a.repo.ListSummariesByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list vehicle summaries by user: %w", err)
	}
	out := make([]telemetry.VehicleCatalogRow, 0, len(rows))
	for i := range rows {
		v := &rows[i]
		out = append(out, telemetry.VehicleCatalogRow{
			ID:             v.ID,
			VIN:            v.VIN,
			Name:           v.Name,
			Model:          v.Model,
			Year:           v.Year,
			Color:          v.Color,
			Status:         string(v.Status),
			ChargeLevel:    v.ChargeLevel,
			EstimatedRange: v.EstimatedRange,
			LastUpdated:    v.LastUpdated,
		})
	}
	return out, nil
}

// vehicleSnapshotAdapter adapts store.VehicleRepo.GetByID to the
// telemetry.VehicleSnapshotReader interface used by the
// GET /api/vehicles/{vehicleId}/snapshot and
// GET /api/vehicles/{vehicleId}/drives handlers (MYR-133). The
// adapter exists so the handlers stay decoupled from internal/store.
//
// GetByID returns the wide read with GPS dual-resolution and
// nav-route blob decryption already applied per
// vehicle_repo_scan.go — exactly what /snapshot needs. The drives
// handler reuses this for the ownership check (it needs the userID
// off the row; GetByID is the cheapest source of truth for an
// arbitrary vehicleId cuid).
type vehicleSnapshotAdapter struct {
	repo *store.VehicleRepo
}

func (a *vehicleSnapshotAdapter) GetByID(ctx context.Context, vehicleID string) (telemetry.VehicleSnapshotRow, error) {
	v, err := a.repo.GetByID(ctx, vehicleID)
	if err != nil {
		return telemetry.VehicleSnapshotRow{}, fmt.Errorf("get vehicle by id: %w", err)
	}
	return telemetry.VehicleSnapshotRow{
		ID:                   v.ID,
		UserID:               v.UserID,
		VIN:                  v.VIN,
		Name:                 v.Name,
		Model:                v.Model,
		Year:                 v.Year,
		Color:                v.Color,
		Status:               string(v.Status),
		ChargeLevel:          v.ChargeLevel,
		EstimatedRange:       v.EstimatedRange,
		ChargeState:          v.ChargeState,
		TimeToFull:           v.TimeToFull,
		Speed:                v.Speed,
		GearPosition:         v.GearPosition,
		Heading:              v.Heading,
		Latitude:             v.Latitude,
		Longitude:            v.Longitude,
		LocationName:         v.LocationName,
		LocationAddress:      v.LocationAddress,
		InteriorTemp:         v.InteriorTemp,
		ExteriorTemp:         v.ExteriorTemp,
		OdometerMiles:        v.OdometerMiles,
		FsdMilesSinceReset:   v.FsdMilesSinceReset,
		DestinationName:      v.DestinationName,
		DestinationAddress:   v.DestinationAddress,
		DestinationLatitude:  v.DestinationLatitude,
		DestinationLongitude: v.DestinationLongitude,
		OriginLatitude:       v.OriginLatitude,
		OriginLongitude:      v.OriginLongitude,
		EtaMinutes:           v.EtaMinutes,
		TripDistRemaining:    v.TripDistRemaining,
		NavRouteCoordinates:  v.NavRouteCoordinates,
		LastUpdated:          v.LastUpdated,
	}, nil
}

// driveListerAdapter adapts store.DriveRepo.ListByVehicleID to the
// telemetry.DriveLister interface used by the drives handler
// (MYR-133). The handler defines its own DriveListCursor /
// DriveListPage / DriveListItem types so it stays decoupled from
// internal/store; this adapter performs the cursor and row
// translations at the boundary.
type driveListerAdapter struct {
	repo *store.DriveRepo
}

func (a *driveListerAdapter) ListByVehicleID(ctx context.Context, vehicleID string, cursor telemetry.DriveListCursor, limit int) (telemetry.DriveListPage, error) {
	page, err := a.repo.ListByVehicleID(ctx, vehicleID, store.DriveListCursor{
		StartTime: cursor.StartTime,
		ID:        cursor.ID,
	}, limit)
	if err != nil {
		return telemetry.DriveListPage{}, fmt.Errorf("list drives by vehicle id: %w", err)
	}
	items := make([]telemetry.DriveListItem, 0, len(page.Items))
	for i := range page.Items {
		d := &page.Items[i]
		items = append(items, telemetry.DriveListItem{
			ID:               d.ID,
			VehicleID:        d.VehicleID,
			StartTime:        d.StartTime,
			EndTime:          d.EndTime,
			Date:             d.Date,
			StartLocation:    d.StartLocation,
			StartAddress:     d.StartAddress,
			EndLocation:      d.EndLocation,
			EndAddress:       d.EndAddress,
			DistanceMiles:    d.DistanceMiles,
			DurationMinutes:  d.DurationMinutes,
			AvgSpeedMph:      d.AvgSpeedMph,
			MaxSpeedMph:      d.MaxSpeedMph,
			StartChargeLevel: d.StartChargeLevel,
			EndChargeLevel:   d.EndChargeLevel,
			FsdMiles:         d.FsdMiles,
			FsdPercentage:    d.FsdPercentage,
			CreatedAt:        d.CreatedAt,
		})
	}
	return telemetry.DriveListPage{Items: items, HasMore: page.HasMore}, nil
}

// driveRouteAdapter adapts store.DriveRepo.GetByID to the
// telemetry.DriveRouteFetcher interface used by the drive-route handler
// (DV-20). GetByID already resolves the routePointsEnc shadow into the
// decrypted RoutePoints, so the handler sees plaintext. store.ErrDriveNotFound
// wraps sdk.ErrNotFound, so passing the error through with %w lets the
// handler map an unknown drive to a 404.
type driveRouteAdapter struct {
	repo *store.DriveRepo
}

func (a *driveRouteAdapter) GetDriveRoute(ctx context.Context, driveID string) (telemetry.DriveRouteData, error) {
	rec, err := a.repo.GetByID(ctx, driveID)
	if err != nil {
		return telemetry.DriveRouteData{}, fmt.Errorf("get drive by id: %w", err)
	}
	return telemetry.DriveRouteData{
		DriveID:     rec.ID,
		VehicleID:   rec.VehicleID,
		RoutePoints: rec.RoutePoints,
	}, nil
}

// teslaTokenAdapter adapts store.AccountRepo to the
// telemetry.TeslaTokenProvider interface, converting the store-layer
// TeslaOAuthToken (Unix epoch) to the telemetry-layer TeslaToken (time.Time).
type teslaTokenAdapter struct {
	repo *store.AccountRepo
}

func (a *teslaTokenAdapter) GetTeslaToken(ctx context.Context, userID string) (telemetry.TeslaToken, error) {
	dbTok, err := a.repo.GetTeslaToken(ctx, userID)
	if err != nil {
		return telemetry.TeslaToken{}, err
	}

	var expiresAt time.Time
	if dbTok.ExpiresAt != nil {
		expiresAt = time.Unix(*dbTok.ExpiresAt, 0)
	}

	return telemetry.TeslaToken{
		AccessToken:  dbTok.AccessToken,
		RefreshToken: dbTok.RefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

// teslaTokenUpdaterAdapter adapts store.AccountRepo to the
// telemetry.TeslaTokenUpdater interface for persisting refreshed tokens.
type teslaTokenUpdaterAdapter struct {
	repo *store.AccountRepo
}

func (a *teslaTokenUpdaterAdapter) UpdateTeslaToken(ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64) error {
	return a.repo.UpdateTeslaToken(ctx, userID, accessToken, refreshToken, expiresAt)
}

// proxyTimeout matches the default Fleet API timeout for consistency.
const proxyTimeout = 30 * time.Second

// proxyHTTPClient returns an *http.Client suitable for calling the
// tesla-http-proxy sidecar. When the proxy URL is a loopback address
// (127.0.0.1, localhost, or ::1), TLS certificate verification is
// skipped because the proxy uses a self-signed cert and traffic never
// leaves the machine. This is the standard pattern for tesla-http-proxy
// sidecar deployments (TeslaMate, Home Assistant, etc.).
//
// For non-loopback URLs, returns nil (FleetAPIClient uses its default client).
func proxyHTTPClient(proxyURL string, logger *slog.Logger) *http.Client {
	u, err := url.Parse(proxyURL)
	if err != nil {
		logger.Warn("invalid proxy URL — using default HTTP client",
			slog.String("proxy_url", proxyURL),
			slog.String("error", err.Error()),
		)
		return nil
	}

	host := u.Hostname()
	ip := net.ParseIP(host)
	isLoopback := host == "localhost" || (ip != nil && ip.IsLoopback())

	if !isLoopback {
		return nil
	}

	logger.Info("proxy on loopback — skipping TLS verification",
		slog.String("proxy_url", proxyURL),
	)

	return &http.Client{
		Timeout: proxyTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //#nosec G402 -- loopback only; guard above ensures non-loopback uses verified TLS
			},
		},
	}
}


// openDriveListerAdapter adapts store.DriveRepo to the
// drives.OpenDriveLister interface used by Detector.Start for orphan
// reconciliation (MYR-146). The drives package defines its own
// OpenDriveRow type so it stays decoupled from internal/store; this
// adapter performs the row-shape translation at the boundary.
//
// MYR-147: also exposes MarkOrphanEnded so the reconciler can close
// older same-VIN orphans synchronously instead of silently dropping
// them. The implementation reuses DriveRepo.Complete with zero stats
// and EndTime=now — the only path that doesn't require non-zero
// distance/duration (which we don't have for a drive that never
// resumed telemetry).
type openDriveListerAdapter struct {
	repo *store.DriveRepo
}

func (a *openDriveListerAdapter) ListOpen(ctx context.Context) ([]drives.OpenDriveRow, error) {
	rows, err := a.repo.ListOpen(ctx)
	if err != nil {
		return nil, fmt.Errorf("list open drives: %w", err)
	}
	out := make([]drives.OpenDriveRow, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, drives.OpenDriveRow{
			ID:               r.ID,
			VIN:              r.VIN,
			StartTime:        r.StartTime,
			StartChargeLevel: r.StartChargeLevel,
		})
	}
	return out, nil
}

// MarkOrphanEnded closes the Drive row identified by driveID with
// zero stats and EndTime set to "now" (RFC3339, UTC). Called by the
// reconciler for older same-VIN orphans — drives that pre-date the
// most recent open drive for the same VIN and are therefore
// definitively over.
//
// We pass zero values for all numeric fields because the row never
// accumulated post-restart telemetry (the in-memory state was lost on
// redeploy and the Detector now reattaches only the LATEST per VIN).
// The endTime column is what unblocks the existing
// "endTime IS NULL OR endTime = ''" ListOpen predicate; the zero
// stats are the honest representation of "we don't know what
// happened between start and now".
func (a *openDriveListerAdapter) MarkOrphanEnded(ctx context.Context, driveID string) error {
	endedAt := time.Now().UTC().Format(time.RFC3339)
	if err := a.repo.Complete(ctx, driveID, store.DriveCompletion{EndTime: endedAt}); err != nil {
		return fmt.Errorf("mark orphan ended (drive_id=%s): %w", driveID, err)
	}
	return nil
}

// vehicleAuthorizerAdapter adapts store.VINCache to the
// telemetry.VehicleAuthorizer interface (data-lifecycle.md §3.5,
// MYR-73). The receiver consults this on every inbound mTLS upgrade
// to reject frames for VINs whose Vehicle row has been deleted. A
// cache miss → DB fetch → cache fill (or cached miss for unknown
// VINs); subsequent calls are O(1).
type vehicleAuthorizerAdapter struct {
	cache *store.VINCache
}

// IsAuthorized returns true when the VIN has a matching Vehicle row.
// errors.Is(err, store.ErrVehicleNotFound) is treated as the
// authoritative "not authorized" signal and reported as (false, nil)
// so the receiver can branch on the boolean alone. Other errors
// (transient DB outages) propagate so the receiver fails open per
// the contract.
func (a *vehicleAuthorizerAdapter) IsAuthorized(ctx context.Context, vin string) (bool, error) {
	if vin == "" {
		return false, nil
	}
	_, err := a.cache.ResolveID(ctx, vin)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, store.ErrVehicleNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("vehicleAuthorizerAdapter.IsAuthorized(vin redacted): %w", err)
}
