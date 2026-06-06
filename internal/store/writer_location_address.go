package store

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/myrobotaxi/telemetry/internal/geocode"
)

// MYR-144 — reverse-geocode the CURRENT vehicle location
// (Vehicle.locationName / Vehicle.locationAddress) so the testbench /
// SDK consumers can render an address ("4220 Tributary Wy") instead of
// raw GPS ("33.086131, -96.852202").
//
// The destination-address path in writer_destination_address.go is
// synchronous: a single REST request stalls the flush only briefly and
// the destination GPS pair lives or dies with that one geocode. The
// current-location path is HOT — every flush window carries a fresh GPS
// pair — so we cannot block the flush on a Mapbox round-trip nor burn
// the rate budget on a stationary vehicle. The strategy here is:
//
//  1. Per-VIN cache that stores the last geocoded (lat, lng), the
//     resulting (name, address), and the wall-clock instant we kicked
//     off the call. The cache also tracks "in flight" so two flushes
//     inside the debounce window do not race two goroutines.
//
//  2. Per-flush guard chain (cheapest checks first):
//     - skip if no (lat, lng) in this update
//     - skip if the position has not moved ≥ LocationGeocodeMinMeters
//       since the cached anchor
//     - skip if the last geocode kicked off <  LocationGeocodeDebounce ago
//     - skip if a goroutine is already in flight for this VIN
//
//  3. On pass, fire one goroutine. Update the cache anchor immediately
//     (so subsequent flushes inside the debounce window short-circuit
//     even before the network call returns). On success, write
//     locationName + locationAddress to the Vehicle row in a follow-up
//     UpdateTelemetry. On error, log at Warn and leave the row alone —
//     a stale address is better than a panicked writer.

// defaultLocationGeocodeDebounce caps the per-VIN reverse-geocode
// frequency for the current-location address. 60s is well below the
// Mapbox free-tier 10 req/s steady-state cap when applied per VIN
// across a small fleet, and matches the writer's default flush cadence
// so one geocode per minute per moving vehicle is the steady state.
const defaultLocationGeocodeDebounce = 60 * time.Second

// defaultLocationGeocodeMinMeters skips geocoding when the new position
// is within ~50m of the cached anchor. Below that distance Mapbox tends
// to return the same address anyway, so the call is wasted budget. The
// threshold is generous enough to absorb residential parking jitter
// (GPS drift on a parked car is routinely 5-20m).
const defaultLocationGeocodeMinMeters = 50.0

// locationGeocodeTimeout caps each per-VIN reverse-geocode call so a
// slow Mapbox endpoint cannot keep a goroutine alive forever. Picked
// to match destGeocodeTimeout so an operator chasing a stuck address
// has one number to remember.
const locationGeocodeTimeout = 5 * time.Second

// locAddrEntry holds the per-VIN debounce state for the current-location
// reverse-geocode. lat/lng anchor the distance check, lastFiredAt
// anchors the time-based debounce, and inFlight prevents racing
// goroutines while a single call is outstanding. The cached name/address
// are recorded for diagnostic completeness; the Vehicle row is the
// source of truth on read.
type locAddrEntry struct {
	lat         float64
	lng         float64
	name        string
	address     string
	lastFiredAt time.Time
	inFlight    bool
}

// scheduleLocationGeocode decides whether the just-flushed update for
// vin warrants a reverse-geocode of its current location and, when it
// does, kicks off an async goroutine that calls the geocoder and writes
// the resolved name/address back to the Vehicle row. Never blocks the
// caller (the flush loop) on Mapbox I/O.
func (w *Writer) scheduleLocationGeocode(vin string, update *VehicleUpdate) {
	if update == nil {
		return
	}
	if update.Latitude == nil || update.Longitude == nil {
		return
	}
	lat := *update.Latitude
	lng := *update.Longitude

	if !w.shouldFireLocationGeocode(vin, lat, lng, time.Now()) {
		return
	}

	w.geocodeWG.Add(1)
	go w.runLocationGeocode(vin, lat, lng)
}

// shouldFireLocationGeocode applies the cheap cache checks (distance,
// time, in-flight) and, when the call would fire, claims the in-flight
// slot for vin and advances lastFiredAt to now. Mutates state under the
// lock to keep the decision and the claim atomic — without this, two
// concurrent flushes could both see "should fire" and double-call
// Mapbox.
func (w *Writer) shouldFireLocationGeocode(vin string, lat, lng float64, now time.Time) bool {
	w.locAddrMu.Lock()
	defer w.locAddrMu.Unlock()

	entry, ok := w.locAddrCache[vin]
	if ok {
		if entry.inFlight {
			return false
		}
		if entry.lat != 0 || entry.lng != 0 {
			if haversineMeters(entry.lat, entry.lng, lat, lng) < w.cfg.LocationGeocodeMinMeters {
				return false
			}
		}
		if now.Sub(entry.lastFiredAt) < w.cfg.LocationGeocodeDebounce {
			return false
		}
	}

	// Claim the slot. The anchor coordinates are advanced inside the
	// success path of runLocationGeocode — we only update the timing
	// and in-flight bits here so a failed call still counts toward the
	// debounce window (prevents a thundering retry against a flaky
	// Mapbox endpoint).
	entry.lastFiredAt = now
	entry.inFlight = true
	w.locAddrCache[vin] = entry
	return true
}

// runLocationGeocode performs the reverse-geocode and, on success,
// writes locationName + locationAddress to the Vehicle row. Runs in its
// own goroutine — tracked by geocodeWG so Stop() can drain in-flight
// calls. Failures are logged at Warn and never propagate.
func (w *Writer) runLocationGeocode(vin string, lat, lng float64) {
	defer w.geocodeWG.Done()
	defer w.clearLocationGeocodeInFlight(vin)

	ctx, cancel := context.WithTimeout(context.Background(), locationGeocodeTimeout)
	defer cancel()

	res, err := w.geocoder.ReverseGeocode(ctx, lat, lng)
	if err != nil {
		// ErrNoResult is the routine "geocoder returned nothing" path
		// (NoopGeocoder, or Mapbox legitimately had no match). Stay
		// quiet — there's nothing actionable for an operator.
		if !errors.Is(err, geocode.ErrNoResult) {
			w.logger.Warn("reverse geocode failed for current location",
				slog.String("vin", redactVIN(vin)),
				slog.String("error", err.Error()),
			)
		}
		return
	}

	w.persistLocationAddress(ctx, vin, lat, lng, res.PlaceName, res.Address)
}

// persistLocationAddress writes the resolved name + address to the
// Vehicle row and, on success, advances the per-VIN cache anchor so the
// next flush within the debounce window short-circuits on the distance
// check. A DB-write failure does NOT roll back the cache anchor — the
// row is intentionally allowed to stay stale until the next flush
// outside the debounce window re-tries. This is consistent with how
// destination-address failures behave.
func (w *Writer) persistLocationAddress(ctx context.Context, vin string, lat, lng float64, name, address string) {
	now := time.Now()
	nameCopy := name
	addrCopy := address
	update := VehicleUpdate{
		LocationName: &nameCopy,
		LocationAddr: &addrCopy,
		LastUpdated:  now,
	}
	if err := w.vehicles.UpdateTelemetry(ctx, vin, update); err != nil {
		w.logger.Warn("failed to write location address",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		return
	}

	w.locAddrMu.Lock()
	defer w.locAddrMu.Unlock()
	entry := w.locAddrCache[vin]
	entry.lat = lat
	entry.lng = lng
	entry.name = name
	entry.address = address
	w.locAddrCache[vin] = entry
}

// clearLocationGeocodeInFlight releases the in-flight guard for vin so
// the next flush outside the debounce window can fire a fresh
// reverse-geocode. Runs in a deferred close-over so a panic inside the
// goroutine cannot wedge the cache.
func (w *Writer) clearLocationGeocodeInFlight(vin string) {
	w.locAddrMu.Lock()
	defer w.locAddrMu.Unlock()
	entry := w.locAddrCache[vin]
	entry.inFlight = false
	w.locAddrCache[vin] = entry
}

// geocodeParkedLocation is the one-shot reverse-geocode invoked from
// handleDriveEnded so the Vehicle row's locationName / locationAddress
// snap to the parked address even when the flush-debounce would
// otherwise skip the call. Synchronous because the drive-end path
// already runs in its own per-event handler goroutine and has a
// per-call deadline (driveOpTimeout); a 5s extra reverse-geocode there
// is harmless. The cache anchor is advanced on success so the next
// regular flush within the debounce window short-circuits.
func (w *Writer) geocodeParkedLocation(ctx context.Context, vin string, lat, lng float64) {
	if lat == 0 && lng == 0 {
		return
	}
	geoCtx, cancel := context.WithTimeout(ctx, locationGeocodeTimeout)
	defer cancel()
	res, err := w.geocoder.ReverseGeocode(geoCtx, lat, lng)
	if err != nil {
		if !errors.Is(err, geocode.ErrNoResult) {
			w.logger.Warn("reverse geocode failed for parked location",
				slog.String("vin", redactVIN(vin)),
				slog.String("error", err.Error()),
			)
		}
		return
	}

	w.markLocationGeocodeFired(vin, time.Now())
	w.persistLocationAddress(ctx, vin, lat, lng, res.PlaceName, res.Address)
}

// markLocationGeocodeFired records "we just fired a geocode for vin at
// now" against the cache so the regular flush-debounce treats the
// parked one-shot as if it were a normal scheduled call. Without this,
// drive-end could cause a regular flush 1s later to fire a redundant
// call (the inverse failure mode of the destination cache).
func (w *Writer) markLocationGeocodeFired(vin string, now time.Time) {
	w.locAddrMu.Lock()
	defer w.locAddrMu.Unlock()
	entry := w.locAddrCache[vin]
	entry.lastFiredAt = now
	w.locAddrCache[vin] = entry
}

// haversineMeters returns the great-circle distance in meters between
// two (lat, lng) coordinates using the haversine formula. The 6_371_000
// constant is the WGS-84 mean Earth radius in meters. Cheap enough
// (~one trig call per dimension) to run on every flush.
func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusMeters = 6_371_000.0
	const deg2rad = math.Pi / 180.0

	dLat := (lat2 - lat1) * deg2rad
	dLng := (lng2 - lng1) * deg2rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*deg2rad)*math.Cos(lat2*deg2rad)*
			math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMeters * c
}
