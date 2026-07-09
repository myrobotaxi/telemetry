package geocode

// Feature ranking for reverse-geocode results. Split out of geocode.go
// (which holds the HTTP client and Geocoder plumbing) when the MYR-206
// neighborhood fallback pushed that file past the 300-line limit.

// buildResult selects the best PlaceName and Address from a Mapbox
// response.
//
// PlaceName is ranked in tiers, and the tier ranking is INDEPENDENT of
// feature order in the response array — candidates for every tier are
// collected in a single pass and the tiers are resolved afterwards, so
// a neighborhood feature listed before a qualifying POI can never
// shadow the POI:
//
//  1. POI: the first POI feature (in Mapbox's relevance order) whose
//     center is within poiDistanceThreshold of the queried coord.
//     Within the tier we trust Mapbox's ranking, not raw distance.
//  2. Neighborhood: the first feature tagged "neighborhood" (MYR-206).
//     No distance gate — reverse geocoding returns the neighborhood
//     CONTAINING the point, and a polygon's centroid is routinely
//     hundreds of meters from a coordinate inside it, so the POI
//     threshold would reject nearly every legitimate match.
//  3. Empty: downstream consumers fall back to the street address.
//
// PlaceName never carries a bare city name: features tagged "locality"
// or "place" (Mapbox's city-level types) are excluded from the
// neighborhood tier even when dual-tagged, because a city-level label
// renders "Dallas → Dallas" on intra-city drives (rejected by client
// QA). Neighborhood is the floor of the fallback chain.
//
// Address: prefers the first address feature's full place_name; if none
// is present, falls back to features[0].place_name so we never return
// an empty address when the API gave us something.
func buildResult(lat, lng float64, features []mapboxFeature) *Result {
	var (
		poiName          string
		neighborhoodName string
		addressLine      string
		haveAddress      bool
	)

	for _, f := range features {
		if poiName == "" && isPOI(f.PlaceType) {
			featLng, featLat := f.Center[0], f.Center[1]
			if haversineMeters(lat, lng, featLat, featLng) <= poiDistanceThreshold {
				poiName = f.Text
			}
		}
		if neighborhoodName == "" && isNeighborhood(f.PlaceType) {
			neighborhoodName = f.Text
		}
		if !haveAddress && containsType(f.PlaceType, "address") {
			addressLine = f.PlaceName
			haveAddress = true
		}
	}

	if !haveAddress {
		addressLine = features[0].PlaceName
	}

	placeName := poiName
	if placeName == "" {
		placeName = neighborhoodName
	}

	return &Result{
		PlaceName: placeName,
		Address:   addressLine,
	}
}

// isPOI reports whether a Mapbox feature's place_type list includes the
// "poi" tag.
func isPOI(types []string) bool {
	return containsType(types, "poi")
}

// isNeighborhood reports whether a Mapbox feature qualifies for the
// neighborhood tier of the PlaceName fallback. A feature must carry the
// "neighborhood" tag AND must not carry a city-level tag ("locality" or
// "place") — dual-tagged features are excluded so locality text can
// never leak into PlaceName (see buildResult's ranking comment).
func isNeighborhood(types []string) bool {
	return containsType(types, "neighborhood") &&
		!containsType(types, "locality") &&
		!containsType(types, "place")
}

// containsType reports whether want appears in the given place_type
// list. Mapbox features routinely carry multiple types
// (e.g. ["poi", "landmark"]), so we cannot rely on the first element.
func containsType(types []string, want string) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}
