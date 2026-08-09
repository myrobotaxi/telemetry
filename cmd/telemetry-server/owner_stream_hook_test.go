package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

type fakeLister struct {
	vehicles []telemetry.FleetVehicle
	err      error
}

func (f *fakeLister) ListVehicles(context.Context, string) ([]telemetry.FleetVehicle, error) {
	return f.vehicles, f.err
}

type fakeUpserter struct {
	inputs  []store.OwnedVehicleInput
	outcome store.VehicleUpsertOutcome
	err     error
	// seededVINs / seededOutcomes record the link-time setup-state seed
	// (MYR-491, widened by MYR-517), so a test can assert both that the row is
	// written on EVERY door and that it carries the honest outcome.
	seededVINs     []string
	seededOutcomes []string
	seedErr        error
}

func (f *fakeUpserter) UpsertOwnedVehicle(_ context.Context, in store.OwnedVehicleInput) (store.VehicleUpsertOutcome, error) {
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return "", f.err
	}
	out := f.outcome
	if out == "" {
		out = store.VehicleOwned
	}
	return out, nil
}

func (f *fakeUpserter) SeedFleetConfigSchedule(_ context.Context, vin, outcome string, _ time.Time) error {
	f.seededVINs = append(f.seededVINs, vin)
	f.seededOutcomes = append(f.seededOutcomes, outcome)
	return f.seedErr
}

type fakePusher struct {
	vins []string
	err  error
}

func (f *fakePusher) PushForVIN(_ context.Context, _, vin string) error {
	f.vins = append(f.vins, vin)
	return f.err
}

func ownedVehicle(id, vin, name string) telemetry.FleetVehicle {
	return telemetry.FleetVehicle{ID: json.Number(id), VIN: vin, DisplayName: name, AccessType: "OWNER"}
}

func driverVehicle(id, vin, name string) telemetry.FleetVehicle {
	return telemetry.FleetVehicle{ID: json.Number(id), VIN: vin, DisplayName: name, AccessType: "DRIVER"}
}

func TestOwnerStreamHook_AfterLink(t *testing.T) {
	ctx := context.Background()
	const validVIN = "5YJ3E1EA7KF000001" // 17 chars

	t.Run("syncs vehicles and pushes config per valid VIN", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{
			ownedVehicle("111", validVIN, "Lunar"),
		}}
		upsert := &fakeUpserter{}
		pusher := &fakePusher{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token")

		if len(upsert.inputs) != 1 || upsert.inputs[0].VIN != validVIN {
			t.Fatalf("upsert inputs = %+v, want one with VIN %s", upsert.inputs, validVIN)
		}
		if upsert.inputs[0].TeslaVehicleID != "111" {
			t.Errorf("teslaVehicleId = %q, want 111", upsert.inputs[0].TeslaVehicleID)
		}
		if len(pusher.vins) != 1 || pusher.vins[0] != validVIN {
			t.Errorf("pushed vins = %v, want [%s]", pusher.vins, validVIN)
		}
	})

	// MYR-491 / MYR-503. Tesla's link-time `missing_key` is the ONLY unambiguous
	// evidence of an unpaired virtual key the server ever receives, and it
	// arrives minutes before the owner opens the app. Logging it and dropping
	// it is what left the setup card blank for the tester who then met a dead
	// Lock button.
	t.Run("records Tesla's link-time missing_key so the setup card can render", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("1", validVIN, "V")}}
		upsert := &fakeUpserter{}
		pusher := &fakePusher{err: &telemetry.SkippedVehicleError{
			RedactedVIN: "***0001", Reason: telemetry.SkipReasonMissingKey,
		}}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token")

		if len(upsert.seededVINs) != 1 || upsert.seededVINs[0] != validVIN {
			t.Fatalf("seeded vins = %v, want [%s]", upsert.seededVINs, validVIN)
		}
		if upsert.seededOutcomes[0] != store.SetupOutcomeAwaitingVirtualKey {
			t.Errorf("seeded outcome = %q, want %q", upsert.seededOutcomes[0], store.SetupOutcomeAwaitingVirtualKey)
		}
	})

	// MYR-517. Every terminal outcome of the link-time push must leave a
	// schedule row, because the row — not the outcome — is what the self-heal
	// machinery keys off. Spencer White's car had none, so his lock tap stamped
	// nothing and the MYR-491 card had nothing to render.
	//
	// The OUTCOME still varies, and must: a push that failed for a reason we do
	// NOT understand must never be reported to the owner as "pair your virtual
	// key". Naming the wrong action is worse than naming none.
	t.Run("seeds a row on every push outcome, with the honest label", func(t *testing.T) {
		tests := []struct {
			name        string
			pusher      fleetConfigPusher
			vin         string
			wantOutcome string
		}{
			{
				name:        "push applied",
				pusher:      &fakePusher{},
				vin:         validVIN,
				wantOutcome: store.SetupOutcomeNone,
			},
			{
				name:        "push failed for an unrecognised reason",
				pusher:      &fakePusher{err: errors.New("502 from the proxy")},
				vin:         validVIN,
				wantOutcome: store.SetupOutcomePushFailed,
			},
			{
				name: "push skipped for a reason that is not a missing key",
				pusher: &fakePusher{err: &telemetry.SkippedVehicleError{
					RedactedVIN: "***0001", Reason: "some_other_reason",
				}},
				vin:         validVIN,
				wantOutcome: store.SetupOutcomePushFailed,
			},
			{
				name:        "no signing proxy configured",
				pusher:      nil,
				vin:         validVIN,
				wantOutcome: store.SetupOutcomeNone,
			},
			{
				name:        "malformed VIN, nothing pushable",
				pusher:      &fakePusher{},
				vin:         "SHORTVIN",
				wantOutcome: store.SetupOutcomeNone,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				lister := &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("1", tt.vin, "V")}}
				upsert := &fakeUpserter{}
				hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: tt.pusher, logger: testLogger()}

				hook.AfterLink(ctx, "cuser1", "token")

				if len(upsert.seededVINs) != 1 || upsert.seededVINs[0] != tt.vin {
					t.Fatalf("seeded vins = %v, want [%s]", upsert.seededVINs, tt.vin)
				}
				if upsert.seededOutcomes[0] != tt.wantOutcome {
					t.Errorf("seeded outcome = %q, want %q", upsert.seededOutcomes[0], tt.wantOutcome)
				}
			})
		}
	})

	// THE TEST IN THE DIRECTION THAT BIT (MYR-517). The expiry defect forces a
	// re-authentication, and the re-auth runs the link over a vehicle the first
	// completed link already provisioned. That door must leave the SAME schedule
	// state a fresh link does — one seed write per link, same VIN, same
	// outcome — because it is the door a first-time owner is most likely to walk
	// through and the one that was never exercised.
	t.Run("a re-link over an already-provisioned vehicle seeds exactly as a fresh link does", func(t *testing.T) {
		newHook := func(upsert *fakeUpserter) *ownerStreamHook {
			return &ownerStreamHook{
				lister: &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("1", validVIN, "Tizzy")}},
				upsert: upsert,
				pusher: &fakePusher{err: &telemetry.SkippedVehicleError{
					RedactedVIN: "***0001", Reason: telemetry.SkipReasonMissingKey,
				}},
				logger: testLogger(),
			}
		}

		fresh := &fakeUpserter{}
		newHook(fresh).AfterLink(ctx, "cuser1", "token")

		// The re-link: the same owner, the same car, an existing "Vehicle" row
		// that UpsertOwnedVehicle reconciles rather than inserts.
		relink := &fakeUpserter{outcome: store.VehicleOwned}
		newHook(relink).AfterLink(ctx, "cuser1", "token2")

		if len(relink.seededVINs) != len(fresh.seededVINs) {
			t.Fatalf("re-link seeded %d rows, fresh link seeded %d — the doors must agree",
				len(relink.seededVINs), len(fresh.seededVINs))
		}
		if len(relink.seededVINs) != 1 || relink.seededVINs[0] != validVIN {
			t.Fatalf("re-link seeded vins = %v, want [%s]", relink.seededVINs, validVIN)
		}
		if relink.seededOutcomes[0] != fresh.seededOutcomes[0] {
			t.Errorf("re-link outcome = %q, fresh outcome = %q — the doors must agree",
				relink.seededOutcomes[0], fresh.seededOutcomes[0])
		}
	})

	// The seed is a card's input, not a gate. Failing an owner's Tesla link
	// because a cosmetic row could not be written would be absurd.
	t.Run("a seed failure never breaks the link", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("1", validVIN, "V")}}
		upsert := &fakeUpserter{seedErr: errors.New("db down")}
		pusher := &fakePusher{err: &telemetry.SkippedVehicleError{
			RedactedVIN: "***0001", Reason: telemetry.SkipReasonNotPaired,
		}}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token") // must not panic, must still provision

		if len(upsert.inputs) != 1 {
			t.Fatalf("upsert inputs = %+v, want the vehicle still provisioned", upsert.inputs)
		}
	})

	t.Run("nil pusher: syncs but never pushes (SAFETY guard)", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("1", validVIN, "V")}}
		upsert := &fakeUpserter{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: nil, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token") // must not panic; no push

		if len(upsert.inputs) != 1 {
			t.Errorf("upsert inputs = %d, want 1", len(upsert.inputs))
		}
	})

	t.Run("malformed VIN is synced but not pushed", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("1", "SHORTVIN", "V")}}
		upsert := &fakeUpserter{}
		pusher := &fakePusher{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token")

		if len(upsert.inputs) != 1 {
			t.Errorf("upsert inputs = %d, want 1", len(upsert.inputs))
		}
		if len(pusher.vins) != 0 {
			t.Errorf("pushed vins = %v, want none (malformed VIN guarded)", pusher.vins)
		}
	})

	t.Run("list failure is swallowed (best-effort)", func(t *testing.T) {
		lister := &fakeLister{err: errors.New("fleet 500")}
		upsert := &fakeUpserter{}
		pusher := &fakePusher{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token") // no panic, no calls

		if len(upsert.inputs) != 0 || len(pusher.vins) != 0 {
			t.Errorf("expected no calls on list failure; upserts=%d pushes=%d", len(upsert.inputs), len(pusher.vins))
		}
	})

	t.Run("upsert failure skips push for that vehicle but continues", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{
			ownedVehicle("1", validVIN, "A"),
		}}
		upsert := &fakeUpserter{err: errors.New("vehicle insert failed")}
		pusher := &fakePusher{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token")

		if len(pusher.vins) != 0 {
			t.Errorf("pushed vins = %v, want none (upsert failed)", pusher.vins)
		}
	})

	t.Run("shared-driver vehicle is NOT provisioned or pushed (ownership filter)", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{
			driverVehicle("1", validVIN, "Someone else's car"),
			ownedVehicle("2", "5YJ3E1EA7KF000002", "Mine"),
		}}
		upsert := &fakeUpserter{}
		pusher := &fakePusher{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token")

		// Only the OWNER vehicle is provisioned + pushed; the DRIVER one is dropped.
		if len(upsert.inputs) != 1 || upsert.inputs[0].TeslaVehicleID != "2" {
			t.Fatalf("upsert inputs = %+v, want only the owned vehicle (id 2)", upsert.inputs)
		}
		if len(pusher.vins) != 1 || pusher.vins[0] != "5YJ3E1EA7KF000002" {
			t.Errorf("pushed vins = %v, want only the owned VIN", pusher.vins)
		}
	})

	t.Run("cross-user teslaVehicleId skip is not pushed", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("1", validVIN, "A")}}
		upsert := &fakeUpserter{outcome: store.VehicleSkippedCrossUser}
		pusher := &fakePusher{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token")

		if len(pusher.vins) != 0 {
			t.Errorf("pushed vins = %v, want none (cross-user vehicle not owned)", pusher.vins)
		}
	})
}

// TestOwnerStreamHook_ReaddVehicle covers the targeted, owner-filtered
// re-provision behind the deliberate re-add (MYR-262). ReaddVehicle re-provisions
// ONLY the single car matching teslaVehicleID and shares the passive sync's
// per-vehicle path (provisionVehicle) — the difference from AfterLink is solely
// that the handler clears the tombstone first (tested in the handler + store).
func TestOwnerStreamHook_ReaddVehicle(t *testing.T) {
	ctx := context.Background()
	const validVIN = "5YJ3E1EA7KF000009" // 17 chars

	t.Run("provisions and pushes only the matching owned car", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{
			ownedVehicle("111", "5YJ3E1EA7KF000111", "Other"),
			ownedVehicle("222", validVIN, "Target"),
		}}
		upsert := &fakeUpserter{}
		pusher := &fakePusher{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		if got := hook.ReaddVehicle(ctx, "cuser1", "token", "222"); !got {
			t.Fatalf("ReaddVehicle = false, want true (owned target provisioned)")
		}
		if len(upsert.inputs) != 1 || upsert.inputs[0].TeslaVehicleID != "222" {
			t.Fatalf("upsert inputs = %+v, want only the target (id 222)", upsert.inputs)
		}
		if len(pusher.vins) != 1 || pusher.vins[0] != validVIN {
			t.Errorf("pushed vins = %v, want only the target VIN", pusher.vins)
		}
	})

	t.Run("shared-driver match is never attached (owner filter)", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{
			driverVehicle("333", validVIN, "SharedCar"),
		}}
		upsert := &fakeUpserter{}
		pusher := &fakePusher{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		if got := hook.ReaddVehicle(ctx, "cuser1", "token", "333"); got {
			t.Errorf("ReaddVehicle = true, want false (caller only shares this car)")
		}
		if len(upsert.inputs) != 0 {
			t.Errorf("upsert inputs = %+v, want none (shared driver must not provision)", upsert.inputs)
		}
	})

	t.Run("target not in caller fleet is a no-op false", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("111", validVIN, "A")}}
		upsert := &fakeUpserter{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: &fakePusher{}, logger: testLogger()}

		if got := hook.ReaddVehicle(ctx, "cuser1", "token", "999"); got {
			t.Errorf("ReaddVehicle = true, want false (target not owned by caller)")
		}
		if len(upsert.inputs) != 0 {
			t.Errorf("upsert inputs = %+v, want none", upsert.inputs)
		}
	})

	t.Run("still-tombstoned target is skipped by the shared upsert gate", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{ownedVehicle("222", validVIN, "T")}}
		upsert := &fakeUpserter{outcome: store.VehicleSkippedTombstoned}
		pusher := &fakePusher{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		if got := hook.ReaddVehicle(ctx, "cuser1", "token", "222"); got {
			t.Errorf("ReaddVehicle = true, want false (tombstone still present → skipped)")
		}
		if len(pusher.vins) != 0 {
			t.Errorf("pushed vins = %v, want none (tombstoned car not pushed)", pusher.vins)
		}
	})

	t.Run("list failure is a best-effort false", func(t *testing.T) {
		lister := &fakeLister{err: errors.New("fleet unavailable")}
		hook := &ownerStreamHook{lister: lister, upsert: &fakeUpserter{}, pusher: &fakePusher{}, logger: testLogger()}
		if got := hook.ReaddVehicle(ctx, "cuser1", "token", "222"); got {
			t.Errorf("ReaddVehicle = true, want false on list error")
		}
	})
}
