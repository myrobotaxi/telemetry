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

// vehicleIDAdapter adapts store.VINCache to the telemetry.VehicleIDLookup
// interface (MYR-316). The service-status monitor works in VINs — that is what
// the telemetry stream and the Fleet API speak — but the service-window columns
// live on a side table keyed by the Prisma vehicle id, so one translation is
// unavoidable. Reuses the same cache as vinResolverAdapter, so it costs no
// extra DB round trip per edge.
type vehicleIDAdapter struct {
	cache *store.VINCache
}

func (a *vehicleIDAdapter) GetVehicleID(ctx context.Context, vin string) (string, error) {
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

// vehicleStatusUpdaterAdapter adapts store.VehicleRepo.UpdateStatus to the
// telemetry.VehicleStatusUpdater interface (MYR-259). The status arrives as
// its string enum value ("in_service") so internal/telemetry stays free of
// an internal/store import; the adapter re-types it to store.VehicleStatus.
type vehicleStatusUpdaterAdapter struct {
	repo *store.VehicleRepo
}

func (a *vehicleStatusUpdaterAdapter) UpdateVehicleStatus(ctx context.Context, vin, status string) error {
	return a.repo.UpdateStatus(ctx, vin, store.VehicleStatus(status))
}

// UpdateVehicleStatusBaseline is the MYR-454 guarded write: it applies only
// over `in_service` / `offline`, so the monitor's connected baseline cannot
// overwrite a `driving` the telemetry fold derived from the live stream.
func (a *vehicleStatusUpdaterAdapter) UpdateVehicleStatusBaseline(ctx context.Context, vin, status string) error {
	return a.repo.UpdateStatusBaseline(ctx, vin, store.VehicleStatus(status))
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
			LicensePlate:   v.LicensePlate,
			Status:         string(v.Status),
			ChargeLevel:    v.ChargeLevel,
			EstimatedRange: v.EstimatedRange,
			LastUpdated:    v.LastUpdated,
			HasActiveRide:  v.HasActiveRide,
			// MYR-316: raw sources; the handler resolves the precedence and
			// the in-service gate.
			ServiceETC:           v.ServiceETC,
			ServiceExpectedEndAt: v.ServiceExpectedEndAt,
			// MYR-342: emitted raw — no precedence, no status gate.
			RideShareEnabled: v.RideShareEnabled,
			// MYR-507/578: the stored label plus the raw badge — the handler
			// resolves the wire label from them (and the VIN), so the catalog
			// and the snapshot still name one car one way.
			TrimLabel: v.TrimLabel,
			Trim:      v.Trim,
			// MYR-581: the owner's first name, already resolved AND already
			// reduced to its first token by the store — this boundary carries a
			// display-ready value, never a full name.
			OwnerFirstName: v.OwnerFirstName,
			// MYR-515: the decrypted position pair; the handler applies the
			// (0,0) sentinel collapse and builds the wire object.
			Latitude:  v.Latitude,
			Longitude: v.Longitude,
			// MYR-491: raw schedule; the handler derives the wire state.
			SetupSchedule: setupScheduleRow(v.SetupSchedule),
		})
	}
	return out, nil
}

// setupScheduleRow maps the store's fleet-config schedule onto the telemetry
// package's copy of the same shape (MYR-491).
//
// It exists in ONE place for the same reason the derivation does: three
// adapters — the owner catalog, the shared catalog, and the snapshot — feed the
// same rule, and a field silently dropped in one of them would show a different
// setup state on the list than on the detail sheet for the same car. The two
// structs are deliberately separate types (internal/telemetry must not import
// internal/store), so nothing but this function keeps them in step.
func setupScheduleRow(s store.SetupSchedule) telemetry.VehicleSetupSchedule {
	return telemetry.VehicleSetupSchedule{
		Present:         s.Present,
		LastOutcome:     s.LastOutcome,
		LastAttemptAt:   s.LastAttemptAt,
		SignedCommandAt: s.SignedCommandAt,
		ForcedRepushAt:  s.ForcedRepushAt,
	}
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

// driveDetailAdapter adapts store.DriveRepo.GetByID to the
// telemetry.DriveDetailFetcher interface used by the drive-detail
// handler (MYR-130). GetByID is the wide read — appropriate for a
// detail endpoint — and returns the full DriveRecord; the adapter
// projects it into DriveDetailData, dropping routePoints (served by the
// separate route endpoint). store.ErrDriveNotFound wraps sdk.ErrNotFound,
// so passing the error through with %w lets the handler map an unknown
// drive to a 404.
type driveDetailAdapter struct {
	repo *store.DriveRepo
}

func (a *driveDetailAdapter) GetDriveDetail(ctx context.Context, driveID string) (telemetry.DriveDetailData, error) {
	rec, err := a.repo.GetByID(ctx, driveID)
	if err != nil {
		return telemetry.DriveDetailData{}, fmt.Errorf("get drive by id: %w", err)
	}
	return telemetry.DriveDetailData{
		ID:               rec.ID,
		VehicleID:        rec.VehicleID,
		StartTime:        rec.StartTime,
		EndTime:          rec.EndTime,
		Date:             rec.Date,
		DistanceMiles:    rec.DistanceMiles,
		DurationMinutes:  rec.DurationMinutes,
		AvgSpeedMph:      rec.AvgSpeedMph,
		MaxSpeedMph:      rec.MaxSpeedMph,
		EnergyUsedKwh:    rec.EnergyUsedKwh,
		StartChargeLevel: rec.StartChargeLevel,
		EndChargeLevel:   rec.EndChargeLevel,
		FsdMiles:         rec.FsdMiles,
		FsdPercentage:    rec.FsdPercentage,
		Interventions:    rec.Interventions,
		StartLocation:    rec.StartLocation,
		StartAddress:     rec.StartAddress,
		EndLocation:      rec.EndLocation,
		EndAddress:       rec.EndAddress,
		CreatedAt:        rec.CreatedAt,
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

// ownerTeardownAdapter adapts store.OwnerTeardown to the
// telemetry.VehicleTeardownWriter interface, mapping the store-layer
// TeardownResult onto the telemetry-layer VehicleTeardownResult at the
// package boundary (MYR-258).
type ownerTeardownAdapter struct {
	teardown *store.OwnerTeardown
}

func (a *ownerTeardownAdapter) RemoveVehicle(ctx context.Context, userID, vehicleID string) (telemetry.VehicleTeardownResult, error) {
	res, err := a.teardown.RemoveVehicle(ctx, userID, vehicleID)
	if err != nil {
		return telemetry.VehicleTeardownResult{}, err
	}
	return telemetry.VehicleTeardownResult{
		Removed:            res.Removed,
		AlreadyGone:        res.AlreadyGone,
		WasLastVehicle:     res.WasLastVehicle,
		TeslaTokensCleared: res.TeslaTokensCleared,
		DriveCount:         res.DriveCount,
	}, nil
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
// MYR-147/MYR-149: also exposes MarkOrphanEnded, which the reconciler
// now calls for EVERY orphan row (the in-memory reattach path was
// removed — see internal/drives/reconcile.go). The implementation
// reuses DriveRepo.Complete with zero stats and EndTime=now — the only
// path that doesn't require non-zero distance/duration (which we don't
// have for a drive whose in-memory state was lost on restart).
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
// reconciler for every orphan row found on startup (MYR-149).
//
// We pass zero values for all numeric fields because the row never
// accumulated post-restart telemetry (the in-memory state was lost on
// redeploy). The endTime column is what unblocks the existing
// "endTime IS NULL OR endTime = ”" ListOpen predicate; the zero
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
