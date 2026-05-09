package events

import "testing"

func TestVehicleDeletedEvent_EventTopic(t *testing.T) {
	evt := VehicleDeletedEvent{VehicleID: "v1", UserID: "u1", VIN: "5YJ3"}
	if got := evt.EventTopic(); got != TopicVehicleDeleted {
		t.Errorf("EventTopic() = %q, want %q", got, TopicVehicleDeleted)
	}
}

func TestVehicleDeletedEvent_NewEvent(t *testing.T) {
	payload := VehicleDeletedEvent{VehicleID: "v1", UserID: "u1", VIN: "5YJ3"}
	evt := NewEvent(payload)
	if evt.Topic != TopicVehicleDeleted {
		t.Errorf("Topic = %q, want %q", evt.Topic, TopicVehicleDeleted)
	}
	got, ok := evt.Payload.(VehicleDeletedEvent)
	if !ok {
		t.Fatalf("Payload type = %T, want VehicleDeletedEvent", evt.Payload)
	}
	if got.VehicleID != "v1" || got.UserID != "u1" || got.VIN != "5YJ3" {
		t.Errorf("Payload = %+v, want VehicleID=v1 UserID=u1 VIN=5YJ3", got)
	}
}
