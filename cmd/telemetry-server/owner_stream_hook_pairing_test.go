package main

import (
	"context"
	"errors"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

type fakePairingNotifier struct{ vins []string }

func (f *fakePairingNotifier) PairingEvidence(vin string) { f.vins = append(f.vins, vin) }

// TestLinkTimePushAppliedIsPairingEvidence is MYR-529's earliest door.
//
// Tesla applies a fleet-telemetry config only to a VIN whose virtual key is
// enrolled — an unpaired car is exactly what comes back as `missing_key` — so a
// link-time push that APPLIED is proof of pairing arriving at second zero of
// the owner's setup. It used to be indistinguishable from "no proxy configured"
// and "malformed VIN", because all three wrote the same empty outcome, so the
// one door that could have started the clock immediately stayed silent.
//
// The seed row's own behaviour is unchanged on every branch, and asserted here
// alongside the signal so a future edit cannot trade one for the other.
func TestLinkTimePushAppliedIsPairingEvidence(t *testing.T) {
	const validVIN = "5YJ3E1EA7KF000001"

	tests := []struct {
		name        string
		pusher      fleetConfigPusher
		wantSignal  bool
		wantOutcome string
	}{
		{
			name:        "the push applied — the key is already paired",
			pusher:      &fakePusher{},
			wantSignal:  true,
			wantOutcome: store.SetupOutcomeNone,
		},
		{
			name: "Tesla skipped the VIN for a missing key — the expected link-time answer",
			pusher: &fakePusher{err: &telemetry.SkippedVehicleError{
				RedactedVIN: "***0001", Reason: telemetry.SkipReasonMissingKey,
			}},
			wantSignal:  false,
			wantOutcome: store.SetupOutcomeAwaitingVirtualKey,
		},
		{
			name:        "the push failed outright",
			pusher:      &fakePusher{err: errors.New("proxy 502")},
			wantSignal:  false,
			wantOutcome: store.SetupOutcomePushFailed,
		},
		{
			name:        "no signing proxy, so nothing was pushed and nothing is proven",
			pusher:      nil,
			wantSignal:  false,
			wantOutcome: store.SetupOutcomeNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upsert := &fakeUpserter{}
			notifier := &fakePairingNotifier{}
			hook := &ownerStreamHook{
				lister:  &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("111", validVIN, "Lunar")}},
				upsert:  upsert,
				pusher:  tt.pusher,
				pairing: notifier,
				logger:  testLogger(),
			}

			hook.AfterLink(context.Background(), "user-1", "tok")

			if got := len(notifier.vins) > 0; got != tt.wantSignal {
				t.Fatalf("signalled = %v (%v), want %v", got, notifier.vins, tt.wantSignal)
			}
			if tt.wantSignal && notifier.vins[0] != validVIN {
				t.Errorf("signalled VIN = %q, want %q", notifier.vins[0], validVIN)
			}
			// The seed is unconditional on every door (MYR-517) and must stay so.
			if len(upsert.seededVINs) != 1 {
				t.Fatalf("seeded %d rows, want exactly 1 on every branch", len(upsert.seededVINs))
			}
			if upsert.seededOutcomes[0] != tt.wantOutcome {
				t.Errorf("seeded outcome = %q, want %q", upsert.seededOutcomes[0], tt.wantOutcome)
			}
		})
	}
}

// TestLinkTimePairingSignalIsOptional: a deployment with no reconciler has
// nobody to tell, and the hook must not reach for a nil notifier.
func TestLinkTimePairingSignalIsOptional(t *testing.T) {
	const validVIN = "5YJ3E1EA7KF000001"
	upsert := &fakeUpserter{}
	hook := &ownerStreamHook{
		lister: &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("111", validVIN, "Lunar")}},
		upsert: upsert,
		pusher: &fakePusher{},
		logger: testLogger(),
	}
	// No panic, and the rest of the door still runs to completion.
	hook.AfterLink(context.Background(), "user-1", "tok")
	if len(upsert.seededVINs) != 1 {
		t.Fatalf("seeded %d rows, want 1 — a missing notifier must not abort the link", len(upsert.seededVINs))
	}
}

// TestBuildOwnerStreamHookGuardsTheNilReconciler is the Go trap this wiring
// could have walked into: a nil *FleetConfigReconciler assigned straight into
// an interface field is a NON-nil interface holding a nil pointer, and the
// hook's own `pairing == nil` guard would sail past it into a nil-receiver
// channel send on the first car that links.
func TestBuildOwnerStreamHookGuardsTheNilReconciler(t *testing.T) {
	hook := buildOwnerStreamHook(&config.Config{}, &fakeUpserter{}, nil, testLogger())
	if hook.pairing != nil {
		t.Fatal("pairing notifier is non-nil for a nil reconciler; that is the typed-nil trap")
	}
}
