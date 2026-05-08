package store

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/geocode"
)

// destGeocodeTimeout caps each per-flush reverse-geocode call so a slow
// or unreachable Mapbox endpoint cannot block the writer's flush loop.
// Set well under the writer's flush interval so a single laggy call
// does not stall successive ticks.
const destGeocodeTimeout = 5 * time.Second

// destAddrEntry pairs a (lat, lng) with the address Mapbox returned for
// that pair. Used to short-circuit redundant geocoder calls when a
// vehicle's destination GPS is unchanged across flush windows.
type destAddrEntry struct {
	lat     float64
	lng     float64
	address string
}

// applyDestinationAddress fills update.DestinationAddress when the flush
// is about to write a destination GPS pair and the cache lacks a fresh
// entry for it. Clears the cache when the navigation atomic group is
// being cleared (NFR-3.3 / vehicle-state-schema.md §3.1 all-or-nothing
// clear). When a flush carries BOTH a clear and a fresh set (the user
// cancelled and immediately re-navigated within one flush window), the
// set wins: the destination columns are stripped from ClearFields, the
// cache is invalidated for the prior destination, and the new GPS pair
// is geocoded so the first flush row carries the new address rather
// than NULL. On geocoder failure (timeout, no result, transport error)
// the address is left nil — the SDK falls back to the raw GPS pair
// (FR-3.4 graceful degradation).
func (w *Writer) applyDestinationAddress(ctx context.Context, vin string, update *VehicleUpdate) {
	clearing := clearsDestination(update.ClearFields)
	hasNewDest := update.DestinationLatitude != nil && update.DestinationLongitude != nil

	if clearing && !hasNewDest {
		w.invalidateDestAddr(vin)
		return
	}
	if clearing && hasNewDest {
		// Coalesced "cancel → re-set" within a single flush window.
		// Strip the destination columns from ClearFields so the SQL
		// builder writes the new values instead of SET NULL'ing them
		// (queries.go's ClearFields loop overrides the value loop), and
		// invalidate the prior cache entry so the new GPS pair is
		// freshly geocoded.
		update.ClearFields = filterOutDestinationColumns(update.ClearFields)
		w.invalidateDestAddr(vin)
	}
	if !hasNewDest {
		return
	}
	if update.DestinationAddress != nil {
		// Caller (or a prior coalesced event) already supplied the
		// address — refresh the cache so a subsequent unchanged flush
		// short-circuits.
		w.cacheDestAddr(vin, *update.DestinationLatitude, *update.DestinationLongitude, *update.DestinationAddress)
		return
	}
	if cached, ok := w.lookupDestAddr(vin); ok &&
		cached.lat == *update.DestinationLatitude &&
		cached.lng == *update.DestinationLongitude {
		addr := cached.address
		update.DestinationAddress = &addr
		return
	}

	geoCtx, cancel := context.WithTimeout(ctx, destGeocodeTimeout)
	defer cancel()
	res, err := w.geocoder.ReverseGeocode(geoCtx, *update.DestinationLatitude, *update.DestinationLongitude)
	if err != nil {
		// ErrNoResult is the routine "geocoder returned nothing" path
		// (NoopGeocoder, or Mapbox legitimately had no match). Log the
		// transport / timeout cases at Warn so an operator chasing a
		// stuck navigation address has a breadcrumb.
		if !errors.Is(err, geocode.ErrNoResult) {
			w.logger.Warn("reverse geocode failed for destination",
				slog.String("vin", redactVIN(vin)),
				slog.String("error", err.Error()),
			)
		}
		return
	}
	addr := res.Address
	update.DestinationAddress = &addr
	w.cacheDestAddr(vin, *update.DestinationLatitude, *update.DestinationLongitude, addr)
}

// clearsDestination reports whether the flushed update is part of an
// active-navigation cancellation (FieldDestLocation Invalid clears
// destinationLatitude + destinationLongitude + destinationAddress per
// field_mapper.navFieldColumns).
func clearsDestination(cols []string) bool {
	for _, c := range cols {
		if c == "destinationLatitude" || c == "destinationLongitude" {
			return true
		}
	}
	return false
}

// filterOutDestinationColumns returns a copy of cols without the three
// destination atomic-group column names. Used when a coalesced flush
// carries both a clear and a re-set: the new set must win, so the
// stripped columns are not written as SET NULL by the SQL builder.
func filterOutDestinationColumns(cols []string) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if c == "destinationLatitude" || c == "destinationLongitude" || c == "destinationAddress" {
			continue
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return slices.Clip(out)
}

// lookupDestAddr returns the cached destination-address entry for vin,
// or false when there is no entry. Safe for concurrent use.
func (w *Writer) lookupDestAddr(vin string) (destAddrEntry, bool) {
	w.destAddrMu.Lock()
	defer w.destAddrMu.Unlock()
	e, ok := w.destAddrCache[vin]
	return e, ok
}

// cacheDestAddr stores a (lat, lng, address) tuple for vin so subsequent
// unchanged flushes skip the geocoder call. Safe for concurrent use.
func (w *Writer) cacheDestAddr(vin string, lat, lng float64, address string) {
	w.destAddrMu.Lock()
	defer w.destAddrMu.Unlock()
	w.destAddrCache[vin] = destAddrEntry{lat: lat, lng: lng, address: address}
}

// invalidateDestAddr drops vin from the cache so the next flush carrying
// destination GPS triggers a fresh geocoder call. Called on
// active-navigation cancellation. Safe for concurrent use.
func (w *Writer) invalidateDestAddr(vin string) {
	w.destAddrMu.Lock()
	defer w.destAddrMu.Unlock()
	delete(w.destAddrCache, vin)
}
