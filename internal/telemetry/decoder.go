package telemetry

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	tpb "github.com/tnando/my-robo-taxi-telemetry/internal/telemetry/proto/tesla"
)

// Decoder converts raw Tesla protobuf payloads into domain-level
// VehicleTelemetryEvent values. It handles the many quirks of Tesla's
// value encoding: numeric fields sent as strings, location values with
// nested lat/lng, typed enums for gear/charge state, and fields whose
// types change across firmware versions.
type Decoder struct{}

// NewDecoder creates a Decoder.
func NewDecoder() *Decoder {
	return &Decoder{}
}

// Decode unmarshals raw bytes into a Tesla Payload protobuf, validates
// required fields, and converts each datum into a TelemetryValue keyed by
// our internal field names. Fields not in fieldMap are silently skipped.
//
// The returned FieldError slice contains per-field decode errors that did
// not prevent the overall payload from being decoded. Callers can log
// these but should still use the event.
func (d *Decoder) Decode(raw []byte) (events.VehicleTelemetryEvent, []FieldError, error) {
	var payload tpb.Payload
	if err := proto.Unmarshal(raw, &payload); err != nil {
		return events.VehicleTelemetryEvent{}, nil, fmt.Errorf("decode protobuf: %w", err)
	}

	return d.DecodePayload(&payload)
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

	// Check the invalid flag first.
	if _, ok := v.Value.(*tpb.Value_Invalid); ok {
		return events.TelemetryValue{}, ErrInvalidValue
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
