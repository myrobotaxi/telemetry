package telemetry

import "errors"

// Sentinel errors for telemetry decoding failures.
var (
	// ErrEmptyPayload indicates a protobuf payload with no data.
	ErrEmptyPayload = errors.New("empty telemetry payload")

	// ErrNilDatum indicates a datum entry in the payload was nil.
	ErrNilDatum = errors.New("nil datum in payload")

	// ErrNilValue indicates a datum had a nil value field.
	ErrNilValue = errors.New("nil value in datum")

	// ErrInvalidValue indicates the datum was explicitly marked invalid by the vehicle.
	ErrInvalidValue = errors.New("datum marked invalid by vehicle")

	// ErrMissingVIN indicates the payload had no VIN.
	ErrMissingVIN = errors.New("missing VIN in payload")

	// ErrMissingTimestamp indicates the payload had no created_at timestamp.
	ErrMissingTimestamp = errors.New("missing timestamp in payload")

	// ErrUnexpectedValueType indicates the value type did not match what
	// was expected for the field. Tesla occasionally changes value types
	// across firmware versions.
	ErrUnexpectedValueType = errors.New("unexpected value type for field")

	// ErrUnsupportedMessageType indicates the FlatBuffers envelope contained
	// a message type other than FlatbuffersStream (type 4).
	ErrUnsupportedMessageType = errors.New("unsupported FlatBuffers message type")

	// ErrUnsupportedTopic indicates the FlatBuffers envelope had a topic
	// that we do not handle (only "V" for vehicle data is supported).
	ErrUnsupportedTopic = errors.New("unsupported envelope topic")
)
