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

// gpsScanResult is the per-pair scan output: the two encrypted-shadow
// halves. Held in a value type so applyResolvedGPS doesn't need a
// half-dozen positional parameters.
//
// MYR-433 removed the plaintext halves from this struct along with the
// columns themselves — the wide vehicle SELECT no longer reads them.
type gpsScanResult struct {
	latEnc, lngEnc *string
}

// scanVehicleRow scans the full SELECT into a Vehicle, applying the
// MYR-63 dual-read GPS resolution AND the MYR-64 nav-route blob
// resolution on the way out. The encrypted-shadow columns are scanned
// into local *string slots, never exposed on the returned struct —
// consumers only see the resolved float64 / RawMessage values.
func (r *VehicleRepo) scanVehicleRow(row rowScanner) (Vehicle, error) {
	return r.scanVehicleRowExtra(row)
}

// scanVehicleRowExtra is the shared scan core. It scans the canonical wide
// vehicleSelectColumns projection and applies the GPS/nav dual-read resolution,
// then scans any `extra` destinations appended to the SELECT after the *Enc
// columns. GetByID passes the MYR-269 control-state columns (added by a LEFT
// JOIN on go_vehicle_control_state) as extra so the snapshot read returns the
// owner controls; GetByVIN / ListByUser pass nothing and are unchanged.
func (r *VehicleRepo) scanVehicleRowExtra(row rowScanner, extra ...any) (Vehicle, error) {
	var v Vehicle
	var status string
	var (
		latEnc, lngEnc             *string
		destLatEnc, destLngEnc     *string
		originLatEnc, originLngEnc *string
		navRouteEnc                *string
	)
	dests := append(make([]any, 0, 33+len(extra)),
		&v.ID, &v.UserID, &v.VIN, &v.Name,
		&v.Model, &v.Year, &v.Color, &v.LicensePlate, &status,
		&v.ChargeLevel, &v.EstimatedRange, &v.ChargeState, &v.TimeToFull,
		&v.Speed, &v.GearPosition,
		&v.Heading,
		&v.LocationName, &v.LocationAddress,
		&v.InteriorTemp, &v.ExteriorTemp,
		&v.OdometerMiles, &v.FsdMilesSinceReset,
		&v.DestinationName, &v.DestinationAddress,
		&v.EtaMinutes, &v.TripDistRemaining,
		&v.LastUpdated,
		&latEnc, &lngEnc,
		&destLatEnc, &destLngEnc,
		&originLatEnc, &originLngEnc,
		&navRouteEnc,
	)
	dests = append(dests, extra...)
	err := row.Scan(dests...)
	if err != nil {
		// Caller is scanVehicle (single-row) or ListByUser (rows
		// iteration). Both wrap with operation context, so we surface
		// the raw scan error here without double-wrapping.
		return Vehicle{}, err //nolint:wrapcheck // wrapped by callers
	}
	v.Status = VehicleStatus(status)
	r.applyResolvedGPS(
		&v,
		gpsScanResult{latEnc, lngEnc},
		gpsScanResult{destLatEnc, destLngEnc},
		gpsScanResult{originLatEnc, originLngEnc},
	)
	r.applyResolvedNavRoute(&v, navRouteEnc)
	return v, nil
}

// applyResolvedNavRoute decrypts `navRouteCoordinatesEnc` into
// `Vehicle.navRouteCoordinates`. The ciphertext column is the only
// source (MYR-433); on any failure the field stays nil, meaning "no
// route", rather than falling back to a plaintext column this server
// no longer reads.
//
// Decrypt failures are logged at Warn so a corrupt 100KB+ blob shows up
// in operator dashboards without 500'ing the live nav-route view.
//
// A repo built without an Encryptor cannot read this field at all — it
// leaves the route nil. That is a real capability loss for the legacy
// constructor and it is intended: no key, no coordinates.
func (r *VehicleRepo) applyResolvedNavRoute(v *Vehicle, ct *string) {
	if r.encryptor == nil || ct == nil || *ct == "" {
		return
	}
	raw, err := routeblob.DecryptJSONBytes(*ct, r.encryptor)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("Vehicle navRouteCoordinatesEnc decrypt failed; surfacing no route",
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
	// surfaces as no route rather than garbage to the SDK.
	if !looksLikeJSONArray(raw) {
		if r.logger != nil {
			r.logger.Warn("Vehicle navRouteCoordinatesEnc decoded to non-array; surfacing no route",
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
// assigns the decrypted values onto v. The main `latitude`/`longitude`
// pair is non-nullable on the Prisma schema (default 0); the Vehicle
// struct surfaces it as float64, so an absent pair collapses to 0.
// Destination and origin pairs are nullable; their resolved pointers
// pass through directly.
//
// A repo built without an Encryptor (legacy NewVehicleRepo path) reads
// no coordinates at all — MYR-433 removed the plaintext columns from the
// projection, so there is nothing for a keyless repo to fall back to.
// Callers that need GPS MUST use NewVehicleRepoWithEncryption.
func (r *VehicleRepo) applyResolvedGPS(v *Vehicle, main, dest, origin gpsScanResult) {
	if r.encryptor == nil {
		return
	}
	mainLat, mainLng := resolveGPSPair(main.latEnc, main.lngEnc, r.encryptor, r.logger, gpsPairs[0])
	v.Latitude = derefFloatOrZero(mainLat)
	v.Longitude = derefFloatOrZero(mainLng)
	v.DestinationLatitude, v.DestinationLongitude = resolveGPSPair(dest.latEnc, dest.lngEnc, r.encryptor, r.logger, gpsPairs[1])
	v.OriginLatitude, v.OriginLongitude = resolveGPSPair(origin.latEnc, origin.lngEnc, r.encryptor, r.logger, gpsPairs[2])
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
