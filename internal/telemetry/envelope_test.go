package telemetry

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// makeTestProtobuf returns a valid protobuf payload for envelope tests.
func makeTestProtobuf(t *testing.T) []byte {
	t.Helper()
	payload := &tpb.Payload{
		Vin:       testVIN,
		CreatedAt: timestamppb.Now(),
		Data: []*tpb.Datum{
			makeDatum(tpb.Field_VehicleSpeed, stringVal("65.2")),
		},
	}
	raw, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal protobuf: %v", err)
	}
	return raw
}

func TestUnwrapEnvelope_ValidStream(t *testing.T) {
	t.Parallel()

	pb := makeTestProtobuf(t)
	envelope := BuildTestEnvelope(testVIN, pb)

	result, err := unwrapEnvelope(envelope)
	if err != nil {
		t.Fatalf("unwrapEnvelope() error = %v", err)
	}

	if result.Topic != topicVehicleData {
		t.Errorf("Topic = %q, want %q", result.Topic, topicVehicleData)
	}
	if result.DeviceID != testVIN {
		t.Errorf("DeviceID = %q, want %q", result.DeviceID, testVIN)
	}
	if len(result.PayloadBytes) == 0 {
		t.Error("PayloadBytes is empty")
	}
}

func TestUnwrapEnvelope_ErrorCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     []byte
		wantSubstr string
	}{
		{name: "nil input", input: nil, wantSubstr: "unwrap envelope"},
		{name: "empty input", input: []byte{}, wantSubstr: "unwrap envelope"},
		{name: "garbage bytes", input: []byte("not a flatbuffers envelope"), wantSubstr: "malformed FlatBuffers"},
		{name: "truncated bytes", input: []byte{0x04, 0x00, 0x00, 0x00}, wantSubstr: "malformed FlatBuffers"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := unwrapEnvelope(tt.input)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantSubstr)
			}
		})
	}
}

func TestUnwrapEnvelope_EmptyPayload(t *testing.T) {
	t.Parallel()

	envelope := BuildTestEnvelope(testVIN, []byte{})
	_, err := unwrapEnvelope(envelope)
	if err == nil {
		t.Fatal("expected error for empty payload, got nil")
	}
	if !errors.Is(err, ErrEmptyPayload) {
		t.Errorf("error = %v, want ErrEmptyPayload", err)
	}
}

func TestUnwrapEnvelope_NonStreamTopic(t *testing.T) {
	t.Parallel()

	pb := makeTestProtobuf(t)
	envelope := buildEnvelope("alerts", testVIN, pb)

	result, err := unwrapEnvelope(envelope)
	if err != nil {
		t.Fatalf("unwrapEnvelope() error = %v", err)
	}

	if result.Topic != "alerts" {
		t.Errorf("Topic = %q, want %q", result.Topic, "alerts")
	}
}

func TestBuildTestEnvelope_Roundtrip(t *testing.T) {
	t.Parallel()

	pb := makeTestProtobuf(t)
	envelope := BuildTestEnvelope(testVIN, pb)

	result, err := unwrapEnvelope(envelope)
	if err != nil {
		t.Fatalf("unwrapEnvelope(BuildTestEnvelope()) error = %v", err)
	}

	// The protobuf bytes should survive the FlatBuffers roundtrip unchanged.
	if len(result.PayloadBytes) != len(pb) {
		t.Errorf("PayloadBytes length = %d, want %d", len(result.PayloadBytes), len(pb))
	}

	// Verify the payload can be unmarshalled back.
	var roundtripped tpb.Payload
	if err := proto.Unmarshal(result.PayloadBytes, &roundtripped); err != nil {
		t.Fatalf("unmarshal roundtripped payload: %v", err)
	}
	if roundtripped.GetVin() != testVIN {
		t.Errorf("roundtripped VIN = %q, want %q", roundtripped.GetVin(), testVIN)
	}
}

func TestDecoder_Decode_UnsupportedTopic(t *testing.T) {
	t.Parallel()
	dec := NewDecoder()

	pb := makeTestProtobuf(t)
	envelope := buildEnvelope("alerts", testVIN, pb)

	_, err := dec.Decode(envelope)
	if err == nil {
		t.Fatal("expected error for unsupported topic, got nil")
	}
	if !errors.Is(err, ErrUnsupportedTopic) {
		t.Errorf("error = %v, want ErrUnsupportedTopic", err)
	}
}
