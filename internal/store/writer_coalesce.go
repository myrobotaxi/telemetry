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

// mergeUpdate applies non-nil fields from src onto dst (latest wins).
func mergeUpdate(dst, src *VehicleUpdate) {
	if src.Speed != nil {
		dst.Speed = src.Speed
	}
	if src.ChargeLevel != nil {
		dst.ChargeLevel = src.ChargeLevel
	}
	if src.EstimatedRange != nil {
		dst.EstimatedRange = src.EstimatedRange
	}
	if src.GearPosition != nil {
		dst.GearPosition = src.GearPosition
	}
	if src.Heading != nil {
		dst.Heading = src.Heading
	}
	if src.Latitude != nil {
		dst.Latitude = src.Latitude
	}
	if src.Longitude != nil {
		dst.Longitude = src.Longitude
	}
	if src.InteriorTemp != nil {
		dst.InteriorTemp = src.InteriorTemp
	}
	if src.ExteriorTemp != nil {
		dst.ExteriorTemp = src.ExteriorTemp
	}
	if src.OdometerMiles != nil {
		dst.OdometerMiles = src.OdometerMiles
	}
	if src.LocationName != nil {
		dst.LocationName = src.LocationName
	}
	if src.LocationAddr != nil {
		dst.LocationAddr = src.LocationAddr
	}
	if src.DestinationName != nil {
		dst.DestinationName = src.DestinationName
	}
	if src.DestinationLatitude != nil {
		dst.DestinationLatitude = src.DestinationLatitude
	}
	if src.DestinationLongitude != nil {
		dst.DestinationLongitude = src.DestinationLongitude
	}
	if src.OriginLatitude != nil {
		dst.OriginLatitude = src.OriginLatitude
	}
	if src.OriginLongitude != nil {
		dst.OriginLongitude = src.OriginLongitude
	}
	if src.EtaMinutes != nil {
		dst.EtaMinutes = src.EtaMinutes
	}
	if src.TripDistRemaining != nil {
		dst.TripDistRemaining = src.TripDistRemaining
	}
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
