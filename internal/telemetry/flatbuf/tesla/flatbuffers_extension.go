// Vendored from github.com/teslamotors/fleet-telemetry/messages/tesla
// to avoid pulling fleet-telemetry's heavy transitive dependencies
// (Kafka, GCP Pub/Sub, ZeroMQ, etc.).

package tesla

import (
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"
)

/******** FlatbuffersEnvelope helpers ********/

// FlatbuffersEnvelopeToBytes serializes a FlatbuffersEnvelope into a finished
// FlatBuffers byte slice.
func FlatbuffersEnvelopeToBytes(
	b *flatbuffers.Builder,
	txid, topic []byte,
	msg flatbuffers.UOffsetT,
	messageID []byte,
	msgType byte,
) []byte {
	txidVector := b.CreateByteString(txid)
	topicVector := b.CreateByteString(topic)
	messageIDVector := b.CreateByteString(messageID)

	FlatbuffersEnvelopeStart(b)
	FlatbuffersEnvelopeAddTxid(b, txidVector)
	FlatbuffersEnvelopeAddTopic(b, topicVector)
	FlatbuffersEnvelopeAddMessageType(b, msgType)
	FlatbuffersEnvelopeAddMessage(b, msg)
	FlatbuffersEnvelopeAddMessageId(b, messageIDVector)
	msgPosition := FlatbuffersEnvelopeEnd(b)

	b.Finish(msgPosition)
	return b.Bytes[b.Head():]
}

// FlatbuffersEnvelopeFromBytes deserializes a FlatbuffersEnvelope from raw
// bytes and extracts the union table for the inner message.
func FlatbuffersEnvelopeFromBytes(value []byte) (*FlatbuffersEnvelope, *flatbuffers.Table, error) {
	if len(value) == 0 {
		return nil, nil, fmt.Errorf("cannot deserialize empty message into FlatbuffersEnvelope")
	}

	envelope := GetRootAsFlatbuffersEnvelope(value, 0)

	unionTable := new(flatbuffers.Table)
	if !envelope.Message(unionTable) {
		return nil, nil, fmt.Errorf("cannot extract message union from FlatbuffersEnvelope")
	}

	return envelope, unionTable, nil
}

/******** FlatbuffersStream helpers ********/

// FlatbuffersStreamToBytes builds a complete FlatBuffers envelope containing a
// FlatbuffersStream message. This is the wire format Tesla vehicles use to
// send telemetry.
func FlatbuffersStreamToBytes(
	senderID, topic, txid, payload []byte,
	createdAt uint32,
	messageID, deviceType, deviceID []byte,
	deliveredAtEpochMs uint64,
) []byte {
	b := flatbuffers.NewBuilder(0)

	senderIDVector := b.CreateByteString(senderID)
	payloadVector := b.CreateByteString(payload)
	deviceTypeVector := b.CreateByteString(deviceType)
	deviceIDVector := b.CreateByteString(deviceID)

	FlatbuffersStreamStart(b)
	FlatbuffersStreamAddSenderId(b, senderIDVector)
	FlatbuffersStreamAddCreatedAt(b, createdAt)
	FlatbuffersStreamAddPayload(b, payloadVector)
	FlatbuffersStreamAddDeviceType(b, deviceTypeVector)
	FlatbuffersStreamAddDeviceId(b, deviceIDVector)
	FlatbuffersStreamAddDeliveredAtEpochMs(b, deliveredAtEpochMs)
	message := FlatbuffersStreamEnd(b)

	return FlatbuffersEnvelopeToBytes(b, txid, topic, message, messageID, MessageFlatbuffersStream)
}
