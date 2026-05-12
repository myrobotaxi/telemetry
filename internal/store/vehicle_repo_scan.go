// Vehicle row-scanning helpers split out of vehicle_repo.go to keep
// each file under the 300-line cap. The scan path applies the MYR-63
// dual-read GPS resolution AND the MYR-64 nav-route blob resolution:
// encrypted-shadow columns are read into local *string slots and
// resolved alongside the plaintext halves so the returned Vehicle
// exposes only the typed shape consumers expect (float64 GPS,
// json.RawMessage navRouteCoordinates).

package store

import (
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/store/routeblob"
)

// rowScanner abstracts pgx.Row vs pgx.Rows so scanVehicleRow can serve
// both single-row queries (GetByVIN/GetByID) and rows iteration
// (ListByUser).
type rowScanner interface {
	Scan(dest ...any) error
}

// gpsScanResult is the per-pair scan output: encrypted-shadow halves
// plus plaintext halves (Float columns; nullable destination/origin
// already arrive as pointers). Held in a value type so applyResolvedGPS
// doesn't need a half-dozen positional parameters.
type gpsScanResult struct {
	latEnc, lngEnc *string
	latPT, lngPT   *float64
}

// scanVehicleRow scans the full SELECT into a Vehicle, applying the
// MYR-63 dual-read GPS resolution AND the MYR-64 nav-route blob
// resolution on the way out. The encrypted-shadow columns are scanned
// into local *string slots, never exposed on the returned struct —
// consumers only see the resolved float64 / RawMessage values.
func (r *VehicleRepo) scanVehicleRow(row rowScanner) (Vehicle, error) {
	var v Vehicle
	var status string
	var (
		latEnc, lngEnc             *string
		destLatEnc, destLngEnc     *string
		originLatEnc, originLngEnc *string
		navRouteEnc                *string
		latPT, lngPT               float64
		destLatPT, destLngPT       *float64
		originLatPT, originLngPT   *float64
	)
	err := row.Scan(
		&v.ID, &v.UserID, &v.VIN, &v.Name,
		&v.Model, &v.Year, &v.Color, &status,
		&v.ChargeLevel, &v.EstimatedRange, &v.ChargeState, &v.TimeToFull,
		&v.Speed, &v.GearPosition,
		&v.Heading, &latPT, &lngPT,
		&v.LocationName, &v.LocationAddress,
		&v.InteriorTemp, &v.ExteriorTemp,
		&v.OdometerMiles, &v.FsdMilesSinceReset,
		&v.DestinationName, &v.DestinationAddress,
		&destLatPT, &destLngPT,
		&originLatPT, &originLngPT,
		&v.EtaMinutes, &v.TripDistRemaining,
		&v.NavRouteCoordinates, &v.LastUpdated,
		&latEnc, &lngEnc,
		&destLatEnc, &destLngEnc,
		&originLatEnc, &originLngEnc,
		&navRouteEnc,
	)
	if err != nil {
		// Caller is scanVehicle (single-row) or ListByUser (rows
		// iteration). Both wrap with operation context, so we surface
		// the raw scan error here without double-wrapping.
		return Vehicle{}, err //nolint:wrapcheck // wrapped by callers
	}
	v.Status = VehicleStatus(status)
	r.applyResolvedGPS(
		&v,
		gpsScanResult{latEnc, lngEnc, &latPT, &lngPT},
		gpsScanResult{destLatEnc, destLngEnc, destLatPT, destLngPT},
		gpsScanResult{originLatEnc, originLngEnc, originLatPT, originLngPT},
	)
	r.applyResolvedNavRoute(&v, navRouteEnc)
	return v, nil
}

// applyResolvedNavRoute prefers the encrypted shadow ciphertext for
// `Vehicle.navRouteCoordinates` and falls back to the plaintext column
// when the shadow is NULL or fails to decrypt/unmarshal. Decrypt
// failures are logged at Warn so a corrupt 100KB+ blob shows up in
// operator dashboards without 500'ing the live nav-route view — same
// fallback policy as the TS counterpart in route-blob-encryption.ts.
//
// Legacy callers built without an Encryptor leave the plaintext
// json.RawMessage untouched.
func (r *VehicleRepo) applyResolvedNavRoute(v *Vehicle, ct *string) {
	if r.encryptor == nil || ct == nil || *ct == "" {
		return
	}
	raw, err := routeblob.DecryptJSONBytes(*ct, r.encryptor)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("Vehicle navRouteCoordinatesEnc decrypt failed; falling back to plaintext",
				slog.String("vehicle_id", v.ID),
				slog.String("error", err.Error()),
			)
		}
		return
	}
	if len(raw) == 0 {
		return
	}
	// Defense-in-depth: ensure the decrypted plaintext is a JSON array
	// before exposing it. A non-array shape is treated as corrupt and
	// falls back to plaintext rather than surfacing garbage to the SDK.
	if !looksLikeJSONArray(raw) {
		if r.logger != nil {
			r.logger.Warn("Vehicle navRouteCoordinatesEnc decoded to non-array; falling back to plaintext",
				slog.String("vehicle_id", v.ID),
			)
		}
		return
	}
	v.NavRouteCoordinates = raw
}

// looksLikeJSONArray is the minimal shape guard the read path applies
// before trusting decrypted nav-route bytes — a full json.Unmarshal
// would force an extra allocation just to confirm `[`. Matches the
// TS-side `Array.isArray(parsed)` guard.
func looksLikeJSONArray(raw []byte) bool {
	for _, b := range raw {
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		return b == '['
	}
	return false
}

// applyResolvedGPS walks the three GPS pairs through resolveGPSPair and
// assigns the resolved values onto v. The main `latitude`/`longitude`
// pair is non-nullable on the Prisma schema (default 0); the Vehicle
// struct surfaces it as float64, so a resolved nil collapses to 0.
// Destination and origin pairs are nullable; their resolved pointers
// pass through directly.
//
// When the repo was built without an Encryptor (legacy NewVehicleRepo
// path) the *Enc columns are ignored entirely — plaintext is the
// source of truth.
func (r *VehicleRepo) applyResolvedGPS(v *Vehicle, main, dest, origin gpsScanResult) {
	if r.encryptor == nil {
		v.Latitude = derefFloatOrZero(main.latPT)
		v.Longitude = derefFloatOrZero(main.lngPT)
		v.DestinationLatitude = dest.latPT
		v.DestinationLongitude = dest.lngPT
		v.OriginLatitude = origin.latPT
		v.OriginLongitude = origin.lngPT
		return
	}
	mainLat, mainLng := resolveGPSPair(main.latEnc, main.lngEnc, main.latPT, main.lngPT, r.encryptor, r.logger, gpsPairs[0])
	v.Latitude = derefFloatOrZero(mainLat)
	v.Longitude = derefFloatOrZero(mainLng)
	v.DestinationLatitude, v.DestinationLongitude = resolveGPSPair(dest.latEnc, dest.lngEnc, dest.latPT, dest.lngPT, r.encryptor, r.logger, gpsPairs[1])
	v.OriginLatitude, v.OriginLongitude = resolveGPSPair(origin.latEnc, origin.lngEnc, origin.latPT, origin.lngPT, r.encryptor, r.logger, gpsPairs[2])
}

// derefFloatOrZero unpacks a *float64 to a float64, returning 0 when
// nil. Used to map the resolved main-GPS pair onto the non-nullable
// Vehicle.Latitude/Longitude struct fields.
func derefFloatOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

