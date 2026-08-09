package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// stubSnapshotReader serves a pre-built row to the real
// VehicleSnapshotHandler so the test can exercise the whole read path
// — projection, response assembly, JSON encoding — without a database.
type stubSnapshotReader struct {
	row telemetry.VehicleSnapshotRow
}

func (s stubSnapshotReader) GetByID(_ context.Context, _ string) (telemetry.VehicleSnapshotRow, error) {
	return s.row, nil
}

// liveDefectVehicle mirrors the production row for the client's own car
// (vehicle cmphman6p000fkz04rq3adktk), whose go_vehicle_control_state row
// carries trim_label='Performance' and fsd_version='FSD (Supervised) v14.3.5'
// alongside the siblings trim='p74d' and color='Quicksilver' that the same
// MYR-320 enrichment writes.
func liveDefectVehicle() store.Vehicle {
	trim := "p74d"
	trimLabel := "Performance"
	fsd := "FSD (Supervised) v14.3.5"
	softwareVersion := "2026.20.1 9a8b7c6"
	return store.Vehicle{
		ID:              "cmphman6p000fkz04rq3adktk",
		UserID:          "user_live_1",
		VIN:             "5YJ3E1EA1NF000320",
		Name:            "Stumpy",
		Model:           "Model Y",
		Year:            2026,
		Color:           "Quicksilver",
		Status:          store.VehicleStatusParked,
		Trim:            &trim,
		TrimLabel:       &trimLabel,
		FSDVersion:      &fsd,
		SoftwareVersion: &softwareVersion,
		LastUpdated:     time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}
}

// TestSnapshotResponse_MYR320DetailsReachTheWire is the live-defect
// reproduction (MYR-320): GET /api/vehicles/{vehicleId}/snapshot returned
// "trimLabel": null and "fsdVersion": null for a car whose DB row holds both
// values, while the siblings written by the SAME enrichment pass (trim, color)
// came through fine.
//
// It drives the REAL VehicleSnapshotHandler over the REAL
// store.Vehicle → VehicleSnapshotRow projection and asserts on the encoded
// JSON body, because every narrower layer already had a green test: the
// migration, the writer, the SELECT, the scan, the response struct's
// toMaskMap and the owner mask table all knew about these two columns. Only
// the cmd/ projection between the store row and the handler did not, and no
// test crossed that boundary.
func TestSnapshotResponse_MYR320DetailsReachTheWire(t *testing.T) {
	v := liveDefectVehicle()

	handler := telemetry.NewVehicleSnapshotHandler(
		&fakeAuth{userID: v.UserID},
		stubSnapshotReader{row: snapshotRowFromVehicle(v)},
		testLogger(),
	)
	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", handler)

	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodGet, "/api/vehicles/"+v.ID+"/snapshot", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode snapshot body: %v", err)
	}

	// The two MYR-320 fields, and the two siblings from the same enrichment
	// that were never broken — asserted together so a regression that takes
	// out the whole detail block is distinguishable from this one.
	for _, tc := range []struct {
		field string
		want  string
	}{
		{"trimLabel", "Performance"},
		{"fsdVersion", "FSD (Supervised) v14.3.5"},
		{"trim", "p74d"},
		{"color", "Quicksilver"},
		{"softwareVersion", "2026.20.1 9a8b7c6"},
	} {
		got, ok := body[tc.field]
		if !ok {
			t.Errorf("snapshot response omits the %q key entirely", tc.field)
			continue
		}
		if got != tc.want {
			t.Errorf("snapshot response %q = %#v, want %q — the store row carries "+
				"the value, so it was dropped on the read path (see "+
				"snapshotRowFromVehicle)", tc.field, got, tc.want)
		}
	}
}

// TestSnapshotRowFromVehicle_NoFieldDropped is the drift gate for the whole
// projection: it fills EVERY field of store.Vehicle with a non-zero value,
// runs the projection, and asserts EVERY field of telemetry.VehicleSnapshotRow
// came out non-zero.
//
// This is the test whose absence let MYR-320 ship half-wired. A field-by-field
// assertion list would have needed the same human to remember the same two
// columns that were forgotten in the struct literal; enumerating by reflection
// cannot forget. Any column added to go_vehicle_control_state from here on
// fails this test until the projection carries it.
func TestSnapshotRowFromVehicle_NoFieldDropped(t *testing.T) {
	src := fullyPopulatedVehicle(t)
	row := snapshotRowFromVehicle(src)

	rv := reflect.ValueOf(row)
	rt := rv.Type()
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		if !rv.Field(i).IsZero() {
			continue
		}
		t.Errorf("VehicleSnapshotRow.%s is the zero value after projecting a "+
			"fully-populated store.Vehicle — snapshotRowFromVehicle "+
			"(adapters_vehicle_snapshot.go) does not copy it, so the "+
			"/snapshot response serves null for a column the DB holds", name)
	}
}

// fullyPopulatedVehicle returns a store.Vehicle whose every field — including
// every nullable pointer — holds a non-zero value, built by reflection so a
// newly added field is populated automatically rather than silently left at
// its zero value (which would make the drift gate above pass vacuously).
func fullyPopulatedVehicle(t *testing.T) store.Vehicle {
	t.Helper()
	var v store.Vehicle
	rv := reflect.ValueOf(&v).Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		field := rt.Field(i)
		if err := fillNonZero(rv.Field(i), field.Name); err != nil {
			t.Fatalf("populate store.Vehicle.%s: %v", field.Name, err)
		}
	}
	return v
}

// fillNonZero writes a non-zero value of the appropriate kind into dst.
// It returns an error rather than panicking on an unhandled kind so a future
// field of an unanticipated type fails LOUDLY here instead of quietly leaving
// a hole in the drift gate.
func fillNonZero(dst reflect.Value, name string) error {
	switch dst.Kind() {
	case reflect.String:
		// Named string types (store.VehicleStatus) take this arm too.
		dst.SetString("v-" + name)
	case reflect.Int, reflect.Int64:
		dst.SetInt(7)
	case reflect.Float64:
		dst.SetFloat(1.5)
	case reflect.Bool:
		dst.SetBool(true)
	case reflect.Slice:
		// json.RawMessage (nav-route coordinates).
		dst.SetBytes([]byte(`[[-122.4194,37.7749]]`))
	case reflect.Pointer:
		p := reflect.New(dst.Type().Elem())
		if err := fillNonZero(p.Elem(), name); err != nil {
			return err
		}
		dst.Set(p)
	case reflect.Struct:
		if dst.Type() == reflect.TypeOf(time.Time{}) {
			dst.Set(reflect.ValueOf(time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)))
			return nil
		}
		// Any other struct is a nested value object on the row —
		// store.SetupSchedule (MYR-491) is the first. RECURSE rather than
		// naming the type: a nested struct whose fields were left at their
		// zero values would make the outer field non-zero and the drift gate
		// pass vacuously, which is the exact failure mode this whole test
		// exists to prevent, one level down.
		for i := range dst.NumField() {
			f := dst.Type().Field(i)
			if !dst.Field(i).CanSet() {
				return fmt.Errorf("unexported field %s.%s cannot be filled", dst.Type(), f.Name)
			}
			if err := fillNonZero(dst.Field(i), name+"."+f.Name); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("no filler for kind %s (type %s)", dst.Kind(), dst.Type())
	}
	return nil
}
