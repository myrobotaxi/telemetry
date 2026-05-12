package telemetry

import (
	"fmt"

	fbtesla "github.com/myrobotaxi/telemetry/internal/telemetry/flatbuf/tesla"
)

// topicVehicleData is the envelope topic for vehicle telemetry protobuf payloads.
const topicVehicleData = "V"

// EnvelopeResult holds the extracted fields from a FlatBuffers envelope after
// unwrapping. The protobuf Payload bytes are in PayloadBytes; the caller is
// responsible for unmarshalling them.
type EnvelopeResult struct {
	// Topic is the routing topic from the envelope (e.g. "V" for vehicle data).
	Topic string

	// DeviceID is the VIN or device identifier from the inner FlatbuffersStream.
	DeviceID string

	// PayloadBytes is the raw protobuf payload extracted from the stream message.
	// NOTE: This is a slice into the FlatBuffers buffer. Decode() unmarshals it
	// immediately, so this is safe. Do not hold a reference after the envelope
	// buffer is garbage collected.
	PayloadBytes []byte

	// TxID is the transaction identifier from the envelope. Useful for future
	// acknowledgment support.
	TxID []byte

	// MessageID is the message identifier from the envelope.
	MessageID []byte
}

// unwrapEnvelope parses a raw FlatBuffers envelope and extracts the inner
// FlatbuffersStream payload. It verifies the message type is FlatbuffersStream
// (type 4) and returns the extracted fields.
//
// The FlatBuffers library panics on malformed input rather than returning
// errors, so this function recovers from panics and converts them to errors.
//
// Returns ErrUnsupportedMessageType if the envelope contains a message type
// other than FlatbuffersStream.
func unwrapEnvelope(raw []byte) (result EnvelopeResult, err error) {
	// The FlatBuffers library does not validate buffer boundaries and will
	// panic (slice bounds out of range, etc.) on malformed input. Convert
	// these panics into a returned error so callers can handle them
	// gracefully.
	defer func() {
		if r := recover(); r != nil {
			result = EnvelopeResult{}
			err = fmt.Errorf("unwrap envelope: malformed FlatBuffers data: %v", r)
		}
	}()

	envelope, unionTable, parseErr := fbtesla.FlatbuffersEnvelopeFromBytes(raw)
	if parseErr != nil {
		return EnvelopeResult{}, fmt.Errorf("unwrap envelope: %w", parseErr)
	}

	msgType := envelope.MessageType()
	if msgType != fbtesla.MessageFlatbuffersStream {
		name := fbtesla.EnumNamesMessage[msgType]
		if name == "" {
			name = fmt.Sprintf("unknown(%d)", msgType)
		}
		return EnvelopeResult{}, fmt.Errorf("unwrap envelope: %w: %s", ErrUnsupportedMessageType, name)
	}

	stream := &fbtesla.FlatbuffersStream{}
	stream.Init(unionTable.Bytes, unionTable.Pos)

	topic := string(envelope.TopicBytes())
	deviceID := string(stream.DeviceIdBytes())
	payload := stream.PayloadBytes()

	if len(payload) == 0 {
		return EnvelopeResult{}, fmt.Errorf("unwrap envelope: %w", ErrEmptyPayload)
	}

	return EnvelopeResult{
		Topic:        topic,
		DeviceID:     deviceID,
		PayloadBytes: payload,
		TxID:         envelope.TxidBytes(),
		MessageID:    envelope.MessageIdBytes(),
	}, nil
}

// buildEnvelope creates a FlatBuffers envelope wrapping a protobuf payload.
// This is primarily used in tests and the simulator to produce realistic
// wire-format messages.
func buildEnvelope(topic, deviceID string, protobufPayload []byte) []byte {
	return fbtesla.FlatbuffersStreamToBytes(
		nil,               // senderID (deprecated)
		[]byte(topic),     // topic
		nil,               // txid
		protobufPayload,   // payload
		0,                 // createdAt (envelope-level, not used)
		nil,               // messageID
		[]byte("vehicle"), // deviceType
		[]byte(deviceID),  // deviceID
		0,                 // deliveredAtEpochMs
	)
}

// BuildTestEnvelope creates a FlatBuffers envelope wrapping a protobuf payload
// with topic "V" (vehicle data). Exported for use in integration tests and the
// simulator.
func BuildTestEnvelope(deviceID string, protobufPayload []byte) []byte {
	return buildEnvelope(topicVehicleData, deviceID, protobufPayload)
}
