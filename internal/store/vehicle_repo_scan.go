// Vehicle row-scanning helpers split out of vehicle_repo.go to keep
// each file under the 300-line cap. The scan path applies the MYR-63
// dual-read GPS resolution: encrypted-shadow columns are read into
// local *string slots and resolved alongside the plaintext halves so
// the returned Vehicle exposes only float64 values.

package store

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
// MYR-63 dual-read GPS resolution on the way out. The encrypted-shadow
// columns are scanned into local *string slots, never exposed on the
// returned struct — consumers only see the resolved float64 values.
func (r *VehicleRepo) scanVehicleRow(row rowScanner) (Vehicle, error) {
	var v Vehicle
	var status string
	var (
		latEnc, lngEnc             *string
		destLatEnc, destLngEnc     *string
		originLatEnc, originLngEnc *string
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
	return v, nil
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

