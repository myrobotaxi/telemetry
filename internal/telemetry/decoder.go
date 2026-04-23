package telemetry

import (
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	tpb "github.com/tnando/my-robo-taxi-telemetry/internal/telemetry/proto/tesla"
)

// Decoder converts raw Tesla FlatBuffers-wrapped protobuf payloads into
// domain-level VehicleTelemetryEvent values. It handles the FlatBuffers
// envelope unwrapping, protobuf deserialization, and the many quirks of
// Tesla's value encoding: numeric fields sent as strings, location values
// with nested lat/lng, typed enums for gear/charge state, and fields whose
// types change across firmware versions.
type Decoder struct{}

// NewDecoder creates a Decoder.
func NewDecoder() *Decoder {
	return &Decoder{}
}

// DecodeResult carries a decoded telemetry event along with metadata from
// the FlatBuffers envelope. The Topic and DeviceID fields come from the
// envelope; the Event is the decoded protobuf payload.
type DecodeResult struct {
	// Event is the decoded telemetry event.
	Event events.VehicleTelemetryEvent

	// FieldErrors contains per-field decode problems that did not prevent
	// the overall payload from being decoded.
	FieldErrors []FieldError

	// Topic is the envelope routing topic (e.g. "V" for vehicle data).
	Topic string

	// DeviceID is the VIN from the FlatBuffers envelope's deviceId field.
	DeviceID string
}

// Decode unwraps a FlatBuffers envelope, extracts the protobuf payload,
// unmarshals it, and converts it into a VehicleTelemetryEvent.
//
// The raw bytes must be a complete FlatBuffers envelope as sent by Tesla
// vehicles over WebSocket. Only topic "V" (vehicle data) is supported;
// other topics return ErrUnsupportedTopic.
//
// The returned FieldError slice (via DecodeResult) contains per-field
// decode errors that did not prevent the overall payload from being
// decoded. Callers can log these but should still use the event.
func (d *Decoder) Decode(raw []byte) (DecodeResult, error) {
	result, _, err := d.decodeCommon(raw, false)
	return result, err
}

// DecodeWithRaw runs the normal decode path and, in the same unmarshal
// pass, also builds a RawVehicleTelemetryEvent with every decoded proto
// field. Used by the dev-only debug endpoint; callers in the hot path
// should use Decode.
func (d *Decoder) DecodeWithRaw(raw []byte) (DecodeResult, events.RawVehicleTelemetryEvent, error) {
	return d.decodeCommon(raw, true)
}

// decodeCommon is the shared envelope-unwrap + protobuf-unmarshal path.
// When includeRaw is true, a RawVehicleTelemetryEvent is also built from
// the same unmarshalled payload.
func (d *Decoder) decodeCommon(raw []byte, includeRaw bool) (DecodeResult, events.RawVehicleTelemetryEvent, error) {
	env, err := unwrapEnvelope(raw)
	if err != nil {
		return DecodeResult{}, events.RawVehicleTelemetryEvent{}, fmt.Errorf("decoder.Decode: %w", err)
	}

	if env.Topic != topicVehicleData {
		return DecodeResult{}, events.RawVehicleTelemetryEvent{}, fmt.Errorf("decoder.Decode(topic=%s): %w", env.Topic, ErrUnsupportedTopic)
	}

	var payload tpb.Payload
	if err := proto.Unmarshal(env.PayloadBytes, &payload); err != nil {
		return DecodeResult{}, events.RawVehicleTelemetryEvent{}, fmt.Errorf("decoder.Decode: decode protobuf: %w", err)
	}

	// Tesla's typed protobuf format often omits the VIN from the protobuf
	// Payload — it's in the FlatBuffers envelope's deviceId instead.
	// Fill it in before validation so DecodePayload doesn't reject it.
	if payload.GetVin() == "" && env.DeviceID != "" {
		payload.Vin = env.DeviceID
	}

	evt, fieldErrs, err := d.DecodePayload(&payload)
	if err != nil {
		return DecodeResult{}, events.RawVehicleTelemetryEvent{}, fmt.Errorf("decoder.Decode: %w", err)
	}

	result := DecodeResult{
		Event:       evt,
		FieldErrors: fieldErrs,
		Topic:       env.Topic,
		DeviceID:    env.DeviceID,
	}

	var rawEvt events.RawVehicleTelemetryEvent
	if includeRaw {
		rawEvt, err = d.DecodeRawPayload(&payload)
		if err != nil {
			return result, events.RawVehicleTelemetryEvent{}, fmt.Errorf("decoder.Decode: %w", err)
		}
	}
	return result, rawEvt, nil
}

// DecodePayload converts an already-unmarshalled Tesla Payload into a
// VehicleTelemetryEvent. This is useful when the caller has already
// deserialized the protobuf (e.g., in tests or from a different transport).
func (d *Decoder) DecodePayload(payload *tpb.Payload) (events.VehicleTelemetryEvent, []FieldError, error) {
	if err := validatePayload(payload); err != nil {
		return events.VehicleTelemetryEvent{}, nil, err
	}

	createdAt := payload.GetCreatedAt().AsTime()
	fields := make(map[string]events.TelemetryValue, len(payload.GetData()))
	var fieldErrors []FieldError

	for _, datum := range payload.GetData() {
		if datum == nil {
			fieldErrors = append(fieldErrors, FieldError{
				Err: ErrNilDatum,
			})
			continue
		}

		// MYR-25: observation-only log for untracked EstimatedHoursToChargeTermination
		// (proto 190). Required until MYR-28's flip-condition Trip Planner capture
		// confirms proto 43 remains the correct source for timeToFull.
		logVerificationField(datum, payload.GetVin())

		name, ok := InternalFieldName(datum.GetKey())
		if !ok {
			continue // untracked field, skip silently
		}

		tv, err := extractValue(datum)
		if err != nil {
			fieldErrors = append(fieldErrors, FieldError{
				Field: name,
				Key:   datum.GetKey(),
				Err:   err,
			})
			continue
		}

		fields[string(name)] = tv

		// MYR-25/29: observation-only log for MilesToArrival (already in fieldMap).
		if name == FieldMilesToArrival {
			logFieldVerification(datum.GetKey().String(), payload.GetVin(), tv)
		}
	}

	evt := events.VehicleTelemetryEvent{
		VIN:       payload.GetVin(),
		CreatedAt: createdAt,
		Fields:    fields,
	}

	return evt, fieldErrors, nil
}

// FieldError records a per-field decode problem. These are non-fatal:
// the rest of the payload may still be usable.
type FieldError struct {
	Field FieldName
	Key   tpb.Field
	Err   error
}

// Error implements the error interface.
func (fe FieldError) Error() string {
	return fmt.Sprintf("field %s (%s): %v", fe.Field, fe.Key, fe.Err)
}

// Unwrap supports errors.Is / errors.As.
func (fe FieldError) Unwrap() error {
	return fe.Err
}

// validatePayload checks that the payload has required fields.
func validatePayload(p *tpb.Payload) error {
	if p == nil {
		return ErrEmptyPayload
	}
	if p.GetVin() == "" {
		return ErrMissingVIN
	}
	if p.GetCreatedAt() == nil {
		return ErrMissingTimestamp
	}
	if len(p.GetData()) == 0 {
		return ErrEmptyPayload
	}
	return nil
}

// extractValue converts a single Tesla Datum into a TelemetryValue.
// Tesla's Value message uses a oneof with many type variants. The key
// challenge is that many numeric fields arrive as string_value on older
// firmware, while newer firmware sends typed values.
func extractValue(datum *tpb.Datum) (events.TelemetryValue, error) {
	v := datum.GetValue()
	if v == nil {
		return events.TelemetryValue{}, ErrNilValue
	}

	// When the vehicle marks a datum as invalid, return a TelemetryValue
	// with Invalid=true instead of an error so downstream consumers can
	// clear stale frontend state (e.g. cancelled nav destinations).
	if _, ok := v.Value.(*tpb.Value_Invalid); ok {
		return events.TelemetryValue{Invalid: true}, nil
	}

	return convertValue(datum.GetKey(), v)
}

// logVerificationField logs raw values for Tesla fields under empirical
// verification. The only remaining observation-only field is
// EstimatedHoursToChargeTermination (proto 190), held out of fieldMap
// pending MYR-25's Trip Planner Supercharger capture (see MYR-28 §7.1
// flip condition). TimeToFullCharge (proto 43) graduated to fieldMap
// once MYR-28 merged; MilesToArrival / MilesSinceReset observation is
// handled inline in DecodePayload.
func logVerificationField(datum *tpb.Datum, vin string) {
	if datum.GetKey() == tpb.Field_EstimatedHoursToChargeTermination {
		if tv, err := extractValue(datum); err == nil {
			logFieldVerification(datum.GetKey().String(), vin, tv)
		}
	}
}

// logFieldVerification emits a structured log line for MYR-25/29 field
// unit verification. Remove after empirical verification is complete.
func logFieldVerification(field, vin string, tv events.TelemetryValue) {
	vinSuffix := vin
	if len(vin) > 4 {
		vinSuffix = vin[len(vin)-4:]
	}
	slog.Info("MYR-25/28/29 FIELD VERIFICATION",
		slog.String("field", field),
		slog.String("vin_last4", vinSuffix),
		slog.Any("raw_value", tv),
	)
}

// DecodeTimestamp extracts a Go time.Time from a Tesla payload's created_at.
// Returns zero time if the timestamp is nil.
func DecodeTimestamp(payload *tpb.Payload) time.Time {
	if payload.GetCreatedAt() == nil {
		return time.Time{}
	}
	return payload.GetCreatedAt().AsTime()
}
