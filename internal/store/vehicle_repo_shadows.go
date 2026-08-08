// Vehicle write-path encryption helpers split out of vehicle_repo.go to
// keep both files under the 300-line cap.
//
// Everything here answers one question: given a VehicleUpdate, what
// ciphertext should this flush write? The output is a map keyed by `*Enc`
// column name that vehicle_update_builder.go turns into SET clauses. The
// map is the entire location write surface of this server — GPS pairs
// (MYR-63/433), the nav-route blob (MYR-64/433) and the geocoded labels
// (MYR-447). Nothing else writes a coordinate or an address.

package store

import (
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/store/routeblob"
)

// buildShadows encrypts the location fields of an update into their
// `*Enc` columns — the ONLY place those values are persisted since
// MYR-433. Returns a map keyed by *Enc column name; a GPS entry exists
// only if both halves of a pair were present in the update.
//
// A nil Encryptor short-circuits to a nil map, which means a repo built
// with the legacy constructor writes no coordinates at all. That is not
// a degraded dual-write any more; it is simply "no key, no location".
//
// Every encrypt failure here is FATAL to the write, for GPS and for the
// nav-route blob alike. The nav-route case used to be non-fatal, on the
// reasoning that the plaintext column still carried the route so live
// telemetry was never lost over an encryption hiccup. With the plaintext
// column gone, continuing would leave the PREVIOUS route ciphertext in
// place while the rest of the row advanced — serving a stale route as
// current — so the write fails and the next telemetry frame retries.
//
//nolint:nilnil // (nil map, nil err) signals "no encryptor wired".
func (r *VehicleRepo) buildShadows(update VehicleUpdate) (map[string]string, error) {
	if r.encryptor == nil {
		return nil, nil
	}
	out := make(map[string]string, 7)
	pairs := []struct {
		pair gpsPair
		lat  *float64
		lng  *float64
	}{
		{gpsPairs[0], update.Latitude, update.Longitude},
		{gpsPairs[1], update.DestinationLatitude, update.DestinationLongitude},
		{gpsPairs[2], update.OriginLatitude, update.OriginLongitude},
	}
	for _, p := range pairs {
		shadow, has, err := buildEncryptedGPSPair(p.lat, p.lng, r.encryptor, r.logger, p.pair)
		if err != nil {
			return nil, fmt.Errorf("encrypt GPS pair %s/%s: %w", p.pair.lat, p.pair.lng, err)
		}
		if !has {
			continue
		}
		out[p.pair.lat+"Enc"] = *shadow.latEnc
		out[p.pair.lng+"Enc"] = *shadow.lngEnc
	}
	if err := r.addNavRouteShadow(update, out); err != nil {
		return nil, err
	}
	if err := r.addLabelShadows(update, out); err != nil {
		return nil, err
	}
	return out, nil
}

// addLabelShadows seals the four MYR-447 location labels into out, keyed
// by `*Enc` column name.
//
// A nil field means the update did not carry that label and the column is
// left untouched. A non-nil EMPTY field is a real instruction — "this car
// has no known address any more" — and is carried through as an empty
// ciphertext, which appendLabelShadowSets writes as NULL.
//
// Fail-closed, like the GPS and nav-route shadows. With the plaintext
// column no longer written there is nowhere else for the label to land,
// so continuing past an encrypt error would leave the PREVIOUS address in
// place while the coordinates beside it advanced — a car shown parked at
// an address it left ten minutes ago. Failing the write lets the next
// telemetry frame retry within seconds.
func (r *VehicleRepo) addLabelShadows(update VehicleUpdate, out map[string]string) error {
	labels := []struct {
		col string
		val *string
	}{
		{"locationName", update.LocationName},
		{"locationAddress", update.LocationAddr},
		{"destinationName", update.DestinationName},
		{"destinationAddress", update.DestinationAddress},
	}
	for _, l := range labels {
		if l.val == nil {
			continue
		}
		ct, err := labelToEncString(*l.val, r.encryptor)
		if err != nil {
			return fmt.Errorf("encrypt label %s: %w", l.col, err)
		}
		out[l.col+"Enc"] = ct
	}
	return nil
}

// addNavRouteShadow encrypts the nav-route blob into out when the
// update touches `Vehicle.navRouteCoordinates`. Empty/null/`[]` raw
// JSON maps to a NULL shadow per routeblob.EncryptJSONBytes.
//
// MYR-433 made an encrypt failure FATAL to the write. Before, it was
// logged at Warn and the write continued, because the plaintext column
// still carried the route — "telemetry is never dropped over a
// shadow-encryption hiccup". That reasoning died with the plaintext
// column. Continuing now would leave the PREVIOUS route ciphertext in
// place while the rest of the row advances, i.e. it would serve a stale
// route as if it were current. Failing the write lets the caller retry
// on the next telemetry frame, which arrives within seconds.
func (r *VehicleRepo) addNavRouteShadow(update VehicleUpdate, out map[string]string) error {
	if update.NavRouteCoordinates == nil {
		return nil
	}
	raw := *update.NavRouteCoordinates
	ct, err := routeblob.EncryptJSONBytes(raw, r.encryptor)
	if err != nil {
		return fmt.Errorf("encrypt navRouteCoordinates: %w", err)
	}
	// ct == "" means "absent" (empty / null / []). Clearing the route is
	// expressed through ClearFields, which NULLs the ciphertext column;
	// we never write an empty ciphertext.
	if ct == "" {
		return nil
	}
	out["navRouteCoordinatesEnc"] = ct
	return nil
}
