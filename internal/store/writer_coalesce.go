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
	// MYR-454: provenance ORs rather than last-write-wins. A single live frame
	// anywhere in the coalesce window is proof the car is streaming, and a
	// REST backfill arriving after it must not demote the merged update and
	// suppress the status fold for the whole batch.
	dst.Streamed = dst.Streamed || src.Streamed
	dst.Speed = mergePtr(dst.Speed, src.Speed)
	dst.ChargeLevel = mergePtr(dst.ChargeLevel, src.ChargeLevel)
	dst.EstimatedRange = mergePtr(dst.EstimatedRange, src.EstimatedRange)
	dst.ChargeState = mergePtr(dst.ChargeState, src.ChargeState)
	dst.TimeToFull = mergePtr(dst.TimeToFull, src.TimeToFull)
	dst.GearPosition = mergePtr(dst.GearPosition, src.GearPosition)
	dst.Heading = mergePtr(dst.Heading, src.Heading)
	dst.Latitude = mergePtr(dst.Latitude, src.Latitude)
	dst.Longitude = mergePtr(dst.Longitude, src.Longitude)
	dst.InteriorTemp = mergePtr(dst.InteriorTemp, src.InteriorTemp)
	dst.ExteriorTemp = mergePtr(dst.ExteriorTemp, src.ExteriorTemp)
	dst.OdometerMiles = mergePtr(dst.OdometerMiles, src.OdometerMiles)
	dst.FsdMilesSinceReset = mergePtr(dst.FsdMilesSinceReset, src.FsdMilesSinceReset)
	dst.LocationName = mergePtr(dst.LocationName, src.LocationName)
	dst.LocationAddr = mergePtr(dst.LocationAddr, src.LocationAddr)
	dst.DestinationName = mergePtr(dst.DestinationName, src.DestinationName)
	dst.DestinationAddress = mergePtr(dst.DestinationAddress, src.DestinationAddress)
	dst.DestinationLatitude = mergePtr(dst.DestinationLatitude, src.DestinationLatitude)
	dst.DestinationLongitude = mergePtr(dst.DestinationLongitude, src.DestinationLongitude)
	dst.OriginLatitude = mergePtr(dst.OriginLatitude, src.OriginLatitude)
	dst.OriginLongitude = mergePtr(dst.OriginLongitude, src.OriginLongitude)
	dst.EtaMinutes = mergePtr(dst.EtaMinutes, src.EtaMinutes)
	dst.TripDistRemaining = mergePtr(dst.TripDistRemaining, src.TripDistRemaining)
	dst.NavRouteCoordinates = mergePtr(dst.NavRouteCoordinates, src.NavRouteCoordinates)

	// MYR-269: merge the owner-control side-table fields (per-field last-write-
	// wins), so a control value survives coalescing within a flush window.
	if src.ControlState != nil {
		if dst.ControlState == nil {
			dst.ControlState = src.ControlState
		} else {
			mergeControlState(dst.ControlState, src.ControlState)
		}
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

	// MYR-409: the nav stamp merges FORWARD-ONLY, and unlike LastUpdated it is
	// frequently zero on one side — a nav frame coalescing with the five
	// motion-only frames around it is the ordinary shape of a flush window. A
	// zero source must therefore not erase a stamp the window already earned
	// (that would report a fresh reading as stale), and a zero destination must
	// take the source's (that is the nav frame arriving second). `After` on a
	// zero src is false and a zero dst is before everything, so both fall out of
	// the same comparison — but only because zero is never a legitimate stamp.
	if src.NavReadingAt.After(dst.NavReadingAt) {
		dst.NavReadingAt = src.NavReadingAt
	}
}
