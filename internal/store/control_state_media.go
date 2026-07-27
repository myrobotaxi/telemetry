package store

import (
	"math"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// controlMediaTextFields maps each MYR-303 free-text now-playing telemetry field
// to the pointer slot it fills on a ControlStateUpdate. Table-driven for the
// same reason controlIntFields is (control_state.go): adding a now-playing line
// is a one-line entry rather than another near-identical if-block, and it makes
// it structurally impossible for one of the five to accidentally get different
// empty-string handling from the other four.
//
// Uses the SAME internal field names the protobuf decoder emits
// (internal/telemetry/fields.go), which for these fields are also the wire
// names in vehicle-state.schema.json.
var controlMediaTextFields = map[telemetry.FieldName]func(*ControlStateUpdate) **string{
	telemetry.FieldMediaTitle:          func(c *ControlStateUpdate) **string { return &c.MediaNowPlayingTitle },
	telemetry.FieldMediaArtist:         func(c *ControlStateUpdate) **string { return &c.MediaNowPlayingArtist },
	telemetry.FieldMediaAlbum:          func(c *ControlStateUpdate) **string { return &c.MediaNowPlayingAlbum },
	telemetry.FieldMediaStation:        func(c *ControlStateUpdate) **string { return &c.MediaNowPlayingStation },
	telemetry.FieldMediaPlaybackSource: func(c *ControlStateUpdate) **string { return &c.MediaPlaybackSource },
}

// controlMediaMsFields maps the two MYR-303 millisecond counters to their
// pointer slots. Kept separate from controlIntFields because these persist as
// *int64 (BIGINT columns) rather than *int — a ms counter is exactly the kind
// of value that should not be one firmware bug away from overflow.
var controlMediaMsFields = map[telemetry.FieldName]func(*ControlStateUpdate) **int64{
	telemetry.FieldMediaDurationMs: func(c *ControlStateUpdate) **int64 { return &c.MediaNowPlayingDuration },
	telemetry.FieldMediaElapsedMs:  func(c *ControlStateUpdate) **int64 { return &c.MediaNowPlayingElapsed },
}

// mapControlMediaNowPlaying derives the MYR-303 now-playing read-backs onto c.
//
// EMPTY-STRING SEMANTICS — the deliberate divergence from MYR-298's
// mediaPlaybackStatus and MYR-274's hvacAutoMode. Both of those DROP an empty or
// "Unknown" value, because they are ENUMS whose "Unknown" member means "we could
// not read this": letting it through would overwrite a known state with an
// admission of ignorance.
//
// The five fields here are FREE TEXT with no Unknown member, and an empty value
// carries the opposite meaning — the car is reporting that the track ENDED and
// nothing is playing now. Dropping it would leave the last known title pinned
// forever, so the app would keep advertising a song that stopped playing an hour
// ago (and, worse, would keep advertising it on a /snapshot long after the car
// went to sleep). So an empty string IS an observation and DOES overwrite; it is
// persisted as an empty string and only a genuinely ABSENT field leaves the
// column NULL. The two remain distinguishable on read: empty means "nothing
// playing", NULL means "never observed".
//
// The numerics use the ordinary rule (a real value including zero overwrites).
// No sentinel handling here: Tesla's 18000000 ms radio placeholder is a real
// emitted value and is stored as-is — special-casing it is the client's job per
// vehicle-state.schema.json, and a server that silently nulled it would destroy
// the client's ability to tell "radio" from "never observed".
//
// None of these fields is emitted by the MYR-260 /vehicle_data backfill
// (vehicleDataToFields) — Tesla's cached vehicle_data carries no now-playing
// block — so this mapping only ever fires for a live streamed frame.
func mapControlMediaNowPlaying(fields map[string]events.TelemetryValue, c *ControlStateUpdate) {
	for field, target := range controlMediaTextFields {
		if v, ok := fields[string(field)]; ok && !v.Invalid && v.StringVal != nil {
			s := *v.StringVal
			*target(c) = &s
		}
	}

	for field, target := range controlMediaMsFields {
		if v, ok := fields[string(field)]; ok && !v.Invalid {
			if ms := controlInt64FromValue(v); ms != nil {
				*target(c) = ms
			}
		}
	}

	if v, ok := fields[string(telemetry.FieldMediaVolumeMax)]; ok && !v.Invalid {
		// Fractional like its sibling mediaVolume — the contract types the
		// volume CEILING as `number`, not `integer`, so mediaVolume /
		// mediaVolumeMax stays exact. Reuses the same extractor.
		if fv := controlFloatFromValue(v); fv != nil {
			c.MediaVolumeMax = fv
		}
	}
}

// mapControlCapability derives the MYR-308 ventilated-seat capability onto c.
// REST-only (vehicle_config.has_seat_cooling via the MYR-260 backfill); Tesla
// streams no such field, so this never fires for a live frame.
//
// A real FALSE is kept, unlike the empty-string drop applied to the MYR-279
// strings: false here is AUTHORITATIVE ("this car has no cooled seats") and is
// exactly the value that lets a client stop offering a control the hardware
// cannot honour. Treating it as absent would silently downgrade an authoritative
// no back to the pre-MYR-308 presence heuristic.
func mapControlCapability(fields map[string]events.TelemetryValue, c *ControlStateUpdate) {
	if v, ok := fields[string(telemetry.FieldSeatCoolingCapable)]; ok && !v.Invalid && v.BoolVal != nil {
		capable := *v.BoolVal
		c.SeatCoolingCapable = &capable
	}
}

// controlInt64FromValue extracts a nullable int64 from a TelemetryValue for the
// millisecond counters. Mirrors controlIntFromValue (control_state.go) but keeps
// the full int64 range: firmware sends these as either IntVal or FloatVal, and a
// float is rounded to nearest, matching the WS layer's roundIfInteger so the
// persisted value equals the live wire value. Returns nil when the value carries
// no number.
func controlInt64FromValue(v events.TelemetryValue) *int64 {
	switch {
	case v.IntVal != nil:
		i := *v.IntVal
		return &i
	case v.FloatVal != nil:
		i := int64(math.Round(*v.FloatVal))
		return &i
	default:
		return nil
	}
}
