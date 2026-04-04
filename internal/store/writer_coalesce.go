package store

import "slices"

// coalesce merges an update into the pending map for the given VIN.
// Returns true if the batch size threshold has been reached.
func (w *Writer) coalesce(vin string, update *VehicleUpdate) bool {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()

	existing, ok := w.pending[vin]
	if !ok {
		w.pending[vin] = update
	} else {
		mergeUpdate(existing, update)
	}
	w.count++
	return w.count >= w.cfg.BatchSize
}

// mergePtr returns src if non-nil, otherwise dst (last-write-wins for pointer fields).
func mergePtr[T any](dst, src *T) *T {
	if src != nil {
		return src
	}
	return dst
}

// mergeUpdate applies non-nil fields from src onto dst (latest wins).
func mergeUpdate(dst, src *VehicleUpdate) {
	dst.Speed = mergePtr(dst.Speed, src.Speed)
	dst.ChargeLevel = mergePtr(dst.ChargeLevel, src.ChargeLevel)
	dst.EstimatedRange = mergePtr(dst.EstimatedRange, src.EstimatedRange)
	dst.GearPosition = mergePtr(dst.GearPosition, src.GearPosition)
	dst.Heading = mergePtr(dst.Heading, src.Heading)
	dst.Latitude = mergePtr(dst.Latitude, src.Latitude)
	dst.Longitude = mergePtr(dst.Longitude, src.Longitude)
	dst.InteriorTemp = mergePtr(dst.InteriorTemp, src.InteriorTemp)
	dst.ExteriorTemp = mergePtr(dst.ExteriorTemp, src.ExteriorTemp)
	dst.OdometerMiles = mergePtr(dst.OdometerMiles, src.OdometerMiles)
	dst.LocationName = mergePtr(dst.LocationName, src.LocationName)
	dst.LocationAddr = mergePtr(dst.LocationAddr, src.LocationAddr)
	dst.DestinationName = mergePtr(dst.DestinationName, src.DestinationName)
	dst.DestinationLatitude = mergePtr(dst.DestinationLatitude, src.DestinationLatitude)
	dst.DestinationLongitude = mergePtr(dst.DestinationLongitude, src.DestinationLongitude)
	dst.OriginLatitude = mergePtr(dst.OriginLatitude, src.OriginLatitude)
	dst.OriginLongitude = mergePtr(dst.OriginLongitude, src.OriginLongitude)
	dst.EtaMinutes = mergePtr(dst.EtaMinutes, src.EtaMinutes)
	dst.TripDistRemaining = mergePtr(dst.TripDistRemaining, src.TripDistRemaining)
	dst.NavRouteCoordinates = mergePtr(dst.NavRouteCoordinates, src.NavRouteCoordinates)

	// Append ClearFields from source so NULL writes survive coalescing.
	// Deduplicate to avoid redundant SET NULL clauses.
	for _, col := range src.ClearFields {
		if !slices.Contains(dst.ClearFields, col) {
			dst.ClearFields = append(dst.ClearFields, col)
		}
	}
	// Always take the later timestamp.
	if src.LastUpdated.After(dst.LastUpdated) {
		dst.LastUpdated = src.LastUpdated
	}
}
