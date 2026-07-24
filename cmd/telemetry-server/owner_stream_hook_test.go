package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
	inputs []store.OwnedVehicleInput
	err    error
}

func (f *fakeUpserter) UpsertOwnedVehicle(_ context.Context, in store.OwnedVehicleInput) error {
	f.inputs = append(f.inputs, in)
	return f.err
}

type fakePusher struct {
	vins []string
	err  error
}

func (f *fakePusher) PushForVIN(_ context.Context, _, vin string) error {
	f.vins = append(f.vins, vin)
	return f.err
}

func fleetVehicle(id, vin, name string) telemetry.FleetVehicle {
	return telemetry.FleetVehicle{ID: json.Number(id), VIN: vin, DisplayName: name}
}

func TestOwnerStreamHook_AfterLink(t *testing.T) {
	ctx := context.Background()
	const validVIN = "5YJ3E1EA7KF000001" // 17 chars

	t.Run("syncs vehicles and pushes config per valid VIN", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{
			fleetVehicle("111", validVIN, "Lunar"),
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

	t.Run("nil pusher: syncs but never pushes (SAFETY guard)", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{fleetVehicle("1", validVIN, "V")}}
		upsert := &fakeUpserter{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: nil, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token") // must not panic; no push

		if len(upsert.inputs) != 1 {
			t.Errorf("upsert inputs = %d, want 1", len(upsert.inputs))
		}
	})

	t.Run("malformed VIN is synced but not pushed", func(t *testing.T) {
		lister := &fakeLister{vehicles: []telemetry.FleetVehicle{fleetVehicle("1", "SHORTVIN", "V")}}
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
			fleetVehicle("1", validVIN, "A"),
		}}
		upsert := &fakeUpserter{err: errors.New("vehicle insert failed")}
		pusher := &fakePusher{}
		hook := &ownerStreamHook{lister: lister, upsert: upsert, pusher: pusher, logger: testLogger()}

		hook.AfterLink(ctx, "cuser1", "token")

		if len(pusher.vins) != 0 {
			t.Errorf("pushed vins = %v, want none (upsert failed)", pusher.vins)
		}
	})
}
