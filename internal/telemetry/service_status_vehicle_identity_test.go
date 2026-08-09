package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// fakeIdentityWriter records every model/year fill so a test can assert both
// WHAT was written and that nothing was written at all in the skip cases.
type fakeIdentityWriter struct {
	mu     sync.Mutex
	calls  int
	users  []string
	vins   []string
	models []string
	years  []int
	filled bool
	err    error
}

func (f *fakeIdentityWriter) FillVehicleIdentity(
	_ context.Context, userID, vin, model string, year int,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.users = append(f.users, userID)
	f.vins = append(f.vins, vin)
	f.models = append(f.models, model)
	f.years = append(f.years, year)
	if f.err != nil {
		return false, f.err
	}
	return f.filled, nil
}

func (f *fakeIdentityWriter) snapshot() (calls int, lastVIN, lastModel string, lastYear int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == 0 {
		return 0, "", "", 0
	}
	last := f.calls - 1
	return f.calls, f.vins[last], f.models[last], f.years[last]
}

// TestSyncVehicleIdentity pins the write rules for the MYR-507 backfill.
//
// The rule that matters most is the mirror of the colour write's empty-skip: a
// VIN position this server does not recognise yields the zero value and is
// NEVER written, because a wrong model on a rider's ride card is worse than the
// blank this issue exists to fix.
//
// Note svcTestVIN is the shape of a real production VIN — position 4 `Y`,
// position 10 `T` — so the expected values here are the same ones the live
// fleet's rows independently carry from the Prisma web-link flow. That
// agreement is the point: two unrelated sources, one answer.
func TestSyncVehicleIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		vin       string
		unwired   bool
		writeErr  error
		notFilled bool
		wantCalls int
		wantModel string
		wantYear  int
	}{
		{
			name:      "a decodable VIN writes both model and year",
			vin:       svcTestVIN,
			wantCalls: 1,
			wantModel: "Model Y",
			wantYear:  2026,
		},
		{
			name: "an UNRECOGNISED model code is never written — and the owner " +
				"lookup is skipped entirely",
			vin: "7SAQGDET7QA613795",
		},
		{
			name: "a malformed VIN is never written",
			vin:  "NOT-A-VIN",
		},
		{
			name: "an empty VIN is never written",
			vin:  "",
		},
		{
			name:    "option not wired: no call at all",
			vin:     svcTestVIN,
			unwired: true,
		},
		{
			// The ordinary steady state: both columns already hold a value, so
			// the statement matches zero rows. Information, not an error.
			name:      "nothing to fill is not an error",
			vin:       svcTestVIN,
			notFilled: true,
			wantCalls: 1,
			wantModel: "Model Y",
			wantYear:  2026,
		},
		{
			name:      "write error is non-fatal",
			vin:       svcTestVIN,
			writeErr:  errors.New("connection reset"),
			wantCalls: 1,
			wantModel: "Model Y",
			wantYear:  2026,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			writer := &fakeIdentityWriter{filled: !tt.notFilled, err: tt.writeErr}
			opts := []ServiceStatusMonitorOption{}
			if !tt.unwired {
				opts = append(opts, WithVehicleIdentity(writer))
			}
			m := newTestMonitor(&fakeVehicleReader{}, &fakeStatusUpdater{}, opts...)

			m.syncVehicleIdentity(context.Background(), tt.vin)

			calls, gotVIN, gotModel, gotYear := writer.snapshot()
			if calls != tt.wantCalls {
				t.Fatalf("identity writes = %d, want %d", calls, tt.wantCalls)
			}
			if calls == 0 {
				return
			}
			if gotVIN != tt.vin {
				t.Errorf("VIN = %q, want %q", gotVIN, tt.vin)
			}
			if gotModel != tt.wantModel {
				t.Errorf("model = %q, want %q", gotModel, tt.wantModel)
			}
			if gotYear != tt.wantYear {
				t.Errorf("year = %d, want %d", gotYear, tt.wantYear)
			}
		})
	}
}

// TestSyncVehicleIdentityNeedsNoTeslaPayload is the property that distinguishes
// this write from the colour write it sits beside, and the reason it is called
// OUTSIDE the `data != nil` guard in refreshVehicleData.
//
// Both values come from the VIN, so a car that answers 200 with an empty body —
// or one that is asleep, offline, or sitting in a service centre having never
// streamed — still gets identified. That is the case the field report was: a
// shared car that had been in the fleet for hours without ever producing the
// connectivity edge an API-sourced enrichment would have waited on.
func TestSyncVehicleIdentityNeedsNoTeslaPayload(t *testing.T) {
	t.Parallel()

	writer := &fakeIdentityWriter{filled: true}
	m := newTestMonitor(&fakeVehicleReader{}, &fakeStatusUpdater{}, WithVehicleIdentity(writer))

	// No VehicleData, no VehicleConfig, no token — nothing but the VIN.
	m.syncVehicleIdentity(context.Background(), svcTestVIN)

	calls, _, gotModel, gotYear := writer.snapshot()
	if calls != 1 {
		t.Fatalf("identity writes = %d, want 1 — the VIN alone must be enough", calls)
	}
	if gotModel != "Model Y" || gotYear != 2026 {
		t.Errorf("wrote (%q, %d), want (\"Model Y\", 2026)", gotModel, gotYear)
	}
}
