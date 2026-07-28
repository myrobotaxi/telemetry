package telemetry

import (
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// defaultStreamFreshness is the per-VIN window during which a live streamed
// frame makes the stream AUTHORITATIVE and suppresses the MYR-260 REST
// /vehicle_data backfill's stream-sourced writes (MYR-300).
//
// Sized to the HvacPower ResendIntervalSeconds (120s, fleet_api_fields.go): a
// car that is genuinely streaming re-emits its comfort/control state at least
// that often, so "we heard from the stream inside the last resend interval"
// is the strongest available evidence that the stream — not Tesla's cached
// REST snapshot — holds the current truth. A car that has gone quiet for
// longer than one resend interval is treated as not streaming and the
// backfill applies normally.
const defaultStreamFreshness = 120 * time.Second

// streamSourcedFields is the set of internal field names the LIVE STREAM can
// source — every value in fieldMap (fields.go). A REST backfill value for one
// of these can overwrite a fresher streamed value through the COALESCE
// control-state upsert, which is exactly the MYR-300 defect, so these are the
// fields the freshness gate drops.
//
// Fields NOT in this set are REST-only (today: `trim`, MYR-279 — Tesla does
// not stream the trim badge; and `seatCoolingCapable`, MYR-308 — a
// vehicle_config spec fact with no proto at all). The stream can never be their
// source, so no stale-overwrite is possible and the gate lets them through even
// while the stream is fresh; otherwise a busily-streaming car could never
// acquire them. For seatCoolingCapable that pass-through is the entire point:
// an in-service car must be able to learn it with no stream whatsoever.
//
// Derived from fieldMap rather than hand-listed so a future field added to
// the decoder is covered automatically.
var streamSourcedFields = buildStreamSourcedFields()

func buildStreamSourcedFields() map[string]struct{} {
	set := make(map[string]struct{}, len(fieldMap))
	for _, name := range fieldMap {
		set[string(name)] = struct{}{}
	}
	return set
}

// WithStreamFreshness overrides the stream-authoritative window — the age
// below which a live streamed frame suppresses the REST backfill's
// stream-sourced writes for that VIN. A non-positive value is ignored (the
// default, defaultStreamFreshness, is kept).
func WithStreamFreshness(d time.Duration) ServiceStatusMonitorOption {
	return func(m *ServiceStatusMonitor) {
		if d > 0 {
			m.freshness = d
		}
	}
}

// noteStreamFrame records a live streamed frame's arrival time for a VIN.
// REST-backfill frames (which the monitor itself publishes onto the same
// topic) are ignored — counting them would let the gate latch on its own
// output and permanently suppress the backfill for a sleeping car.
func (m *ServiceStatusMonitor) noteStreamFrame(evt events.VehicleTelemetryEvent) {
	if !evt.Streamed() {
		return
	}
	m.lastStream.Store(evt.VIN, m.now())
}

// LastStreamAt reports when the most recent LIVE streamed frame arrived for
// this VIN, and whether that frame is still inside the stream-authoritative
// freshness window. A VIN we have never heard from returns the zero time and
// false.
//
// Exported for the MYR-315 owner-triggered refresh endpoint (rest-api.md
// §7.15), which short-circuits to `{"status":"fresh"}` — no Tesla call at all —
// when the stream already holds current truth, and echoes the returned instant
// as the response's `lastUpdated`. Reusing the monitor's per-VIN state rather
// than keeping a second copy is what keeps the endpoint's notion of "fresh"
// identical to the MYR-300 backfill gate's.
func (m *ServiceStatusMonitor) LastStreamAt(vin string) (time.Time, bool) {
	v, ok := m.lastStream.Load(vin)
	if !ok {
		return time.Time{}, false
	}
	last, ok := v.(time.Time)
	if !ok {
		return time.Time{}, false
	}
	return last, m.now().Sub(last) < m.freshness
}

// streamFresh reports whether a live streamed frame has arrived for this VIN
// within the freshness window. The boundary is EXCLUSIVE of the window edge:
// an age of exactly defaultStreamFreshness counts as stale, so a car whose
// resend cadence has lapsed is backfilled rather than left unrefreshed.
func (m *ServiceStatusMonitor) streamFresh(vin string) bool {
	_, fresh := m.LastStreamAt(vin)
	return fresh
}

// dropStreamSourcedFields removes every stream-sourceable field from a REST
// backfill frame, returning the number dropped. Called only when the stream
// is fresh for the VIN: the stream is then the authoritative, current source
// for all of them (locked, trunk/frunk via doorState, hvacPower, temps,
// chargeState, chargePortDoorOpen, soc, odometer, version), and Tesla's
// /vehicle_data can serve a stale cached snapshot that would otherwise
// overwrite fresher streamed state through the COALESCE upsert.
//
// The rule is deliberately field-set-wide rather than hvacPower-only: the
// same stale-cache overwrite can hit any control field the backfill writes.
func dropStreamSourcedFields(fields map[string]events.TelemetryValue) int {
	dropped := 0
	for name := range fields {
		if _, streamed := streamSourcedFields[name]; streamed {
			delete(fields, name)
			dropped++
		}
	}
	return dropped
}
