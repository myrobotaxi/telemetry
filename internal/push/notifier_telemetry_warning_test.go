package push

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-592 — the day-4 inactivity warning, and the transactional-delivery rule
// it introduced.

// silencedPrefs is a PrefStore with every category OFF. A transactional notice
// must sail past it; anything else must not.
type silencedPrefs struct{}

func (silencedPrefs) PrefsForUser(_ context.Context, _ string) (Prefs, error) {
	return Prefs{}, nil
}

func telemetryWarningEvent(ownerID, vehicleName string) events.Event {
	return events.NewEvent(events.VehicleTelemetryWarningEvent{
		OwnerID:     ownerID,
		VehicleID:   "veh_1",
		VehicleName: vehicleName,
		SuspendAt:   time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
	})
}

func TestTelemetryWarningAlert_Copy(t *testing.T) {
	tests := []struct {
		name      string
		vehicle   string
		wantTitle string
		reason    string
	}{
		{
			name:      "a named car is named",
			vehicle:   "Optimus",
			wantTitle: "Optimus will be disconnected tomorrow",
			reason:    "the nickname is the only interpolation the payload policy allows, and it is the one that makes the notice legible",
		},
		{
			name:      "an unnamed car falls back to a sentence subject",
			vehicle:   "",
			wantTitle: "Your car will be disconnected tomorrow",
			reason:    "vehicleLabel's fallback must read as a subject, not as a noun phrase",
		},
		{
			name:      "whitespace is not a name",
			vehicle:   "   ",
			wantTitle: "Your car will be disconnected tomorrow",
			reason:    "a blank title would ship a leading space to a lock screen",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := telemetryWarningAlert(tc.vehicle)
			if got.title != tc.wantTitle {
				t.Errorf("title = %q, want %q (%s)", got.title, tc.wantTitle, tc.reason)
			}
			if got.body != "Open the app to keep it connected." {
				t.Errorf("body = %q, want the action that prevents the disconnect", got.body)
			}
			// The body must name the fix rather than the consequence: this is
			// the only alert in the file about something that has not happened.
			if !strings.Contains(got.body, "Open the app") {
				t.Error("the body no longer tells the owner what to do about it")
			}
		})
	}
}

func TestNotifier_TelemetryWarning_IsTransactionalAndOwnerOnly(t *testing.T) {
	tests := []struct {
		name      string
		prefs     PrefStore
		ownerID   string
		wantSends int
		reason    string
	}{
		{
			name:      "delivered with every category silenced",
			prefs:     silencedPrefs{},
			ownerID:   "user_owner",
			wantSends: 1,
			reason: "an operational account notice answers to no switch — the alternative channel is " +
				"an in-app notice reaching somebody who has not opened the app in four days",
		},
		{
			name:      "delivered with no preference store wired",
			prefs:     nil,
			ownerID:   "user_owner",
			wantSends: 1,
			reason:    "the ordinary unwired state must behave the same as the silenced one for this delivery",
		},
		{
			name:      "an ownerless warning is dropped, not broadcast",
			prefs:     nil,
			ownerID:   "",
			wantSends: 0,
			reason:    "a producer bug must not turn into a fan-out to nobody-in-particular",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sender := NewFakeSender()
			devices := newFakeDeviceStore()
			devices.byUser["user_owner"] = []Device{{Token: "tok_owner"}}
			n := NewNotifier(sender, devices, tc.prefs, nil, nil, Config{Enabled: true}, nil)

			n.handleTelemetryWarning(telemetryWarningEvent(tc.ownerID, "Optimus"))
			n.Wait()

			sent := sender.Sent()
			if len(sent) != tc.wantSends {
				t.Fatalf("sends = %d, want %d (%s)", len(sent), tc.wantSends, tc.reason)
			}
			if tc.wantSends == 0 {
				return
			}
			if sent[0].DeviceToken != "tok_owner" {
				t.Errorf("recipient token = %q, want the OWNER's — a rider cannot act on this and must not be alarmed by it",
					sent[0].DeviceToken)
			}
			if sent[0].RideID != "" {
				t.Errorf("RideID = %q, want empty: this is not a ride, and a borrowed id would collapse "+
					"the warning against unrelated news and reach the client as a rideId pointing at nothing",
					sent[0].RideID)
			}
			if sent[0].EventTopic != string(events.TopicVehicleTelemetryWarning) {
				t.Errorf("EventTopic = %q, want %q", sent[0].EventTopic, events.TopicVehicleTelemetryWarning)
			}
		})
	}
}

// TestFanOut_RefusesADeliveryThatDeclaresNeither closes the silent bypass that
// existed before MYR-592: Prefs.Allows returns true for an unrecognised
// category, so an empty category looked exactly like an authorised transactional
// send.
func TestFanOut_RefusesADeliveryThatDeclaresNeither(t *testing.T) {
	sender := NewFakeSender()
	devices := newFakeDeviceStore()
	devices.byUser["user_a"] = []Device{{Token: "tok_a"}}
	n := NewNotifier(sender, devices, silencedPrefs{}, nil, nil, Config{Enabled: true}, nil)

	n.fanOut(context.Background(), delivery{
		userID: "user_a",
		topic:  "some.topic",
		// No category, not transactional.
	}, alert{title: "t", body: "b"})

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sends = %d, want 0 — a delivery must declare a category or transactional intent", got)
	}
}

// TestFanOut_PreferenceGateStillBindsForOrdinaryDeliveries guards the other
// direction: adding the bypass must not have widened it.
func TestFanOut_PreferenceGateStillBindsForOrdinaryDeliveries(t *testing.T) {
	sender := NewFakeSender()
	devices := newFakeDeviceStore()
	devices.byUser["user_a"] = []Device{{Token: "tok_a"}}
	n := NewNotifier(sender, devices, silencedPrefs{}, nil, nil, Config{Enabled: true}, nil)

	n.fanOut(context.Background(), delivery{
		userID:   "user_a",
		rideID:   "ride_1",
		topic:    string(events.TopicRideDue),
		category: CategoryRideLifecycle,
	}, alert{title: "t", body: "b"})

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sends = %d, want 0 — a silenced ride category must still silence", got)
	}
}
