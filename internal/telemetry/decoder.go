package telemetry

import (
	"fmt"
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
	env, err := unwrapEnvelope(raw)
	if err != nil {
		return DecodeResult{}, fmt.Errorf("decoder.Decode: %w", err)
	}

	if env.Topic != topicVehicleData {
		return DecodeResult{}, fmt.Errorf("decoder.Decode(topic=%s): %w", env.Topic, ErrUnsupportedTopic)
	}

	var payload tpb.Payload
	if err := proto.Unmarshal(env.PayloadBytes, &payload); err != nil {
		return DecodeResult{}, fmt.Errorf("decoder.Decode: decode protobuf: %w", err)
	}

	// Tesla's typed protobuf format often omits the VIN from the protobuf
	// Payload — it's in the FlatBuffers envelope's deviceId instead.
	// Fill it in before validation so DecodePayload doesn't reject it.
	if payload.GetVin() == "" && env.DeviceID != "" {
		payload.Vin = env.DeviceID
	}

	evt, fieldErrs, err := d.DecodePayload(&payload)
	if err != nil {
		return DecodeResult{}, fmt.Errorf("decoder.Decode: %w", err)
	}

	return DecodeResult{
		Event:       evt,
		FieldErrors: fieldErrs,
		Topic:       env.Topic,
		DeviceID:    env.DeviceID,
	}, nil
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

// DecodeTimestamp extracts a Go time.Time from a Tesla payload's created_at.
// Returns zero time if the timestamp is nil.
func DecodeTimestamp(payload *tpb.Payload) time.Time {
	if payload.GetCreatedAt() == nil {
		return time.Time{}
	}
	return payload.GetCreatedAt().AsTime()
}
