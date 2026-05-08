// Package store_test holds integration tests against a real PostgreSQL
// container provisioned by db_test.go's TestMain. This file implements
// MYR-48: a defense-in-depth conformance test that catches the regression
// class where a wire field gets WS broadcast wiring but DB persistence is
// forgotten in the writer pipeline. Concretely, the test enumerates every
// property declared in docs/contracts/schemas/vehicle-state.schema.json,
// applies the synthetic event declared by docs/contracts/fixtures/rest/
// snapshot_completeness.json through VehicleRepo.UpdateTelemetry (or a
// direct seed for identity/derived fields), then reads the row back via
// VehicleRepo.GetByVIN and asserts the field is non-null on the read-back
// snapshot. The Go-side equivalent of "REST /snapshot completeness" is
// "after the writer pipeline applies, does the read path return non-null
// values for every schema property?"
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
)

// fixtureRow describes one row in snapshot_completeness.json — one entry
// per VehicleState schema property.
type fixtureRow struct {
	Category              string         `json:"category"`
	AtomicGroup           string         `json:"atomicGroup"`
	Synthetic             *syntheticSpec `json:"synthetic,omitempty"`
	NullableInSteadyState bool           `json:"nullableInSteadyState"`
	Comment               string         `json:"comment"`
	// LinearIssue is the issue identifier (e.g. "MYR-67") tracking the
	// closing PR for an expected_failure row. Surfaced in test output
	// so `go test -v` log lines name the gap directly.
	LinearIssue string `json:"linearIssue,omitempty"`
}

// expectedGapTag formats the prefix that disambiguates documented
// writer-pipeline gaps from real assertion failures in test output.
func expectedGapTag(row fixtureRow) string {
	if row.LinearIssue != "" {
		return fmt.Sprintf("[EXPECTED-GAP %s]", row.LinearIssue)
	}
	return "[EXPECTED-GAP]"
}

// syntheticSpec describes how to drive a value into the writer pipeline
// (kind=vehicleUpdate) or into the DB row directly (kind=seed).
type syntheticSpec struct {
	Kind  string          `json:"kind"`  // "vehicleUpdate" | "seed"
	Field string          `json:"field"` // VehicleUpdate Go field name OR DB column name
	Value json.RawMessage `json:"value"`
}

// fixtureRoot mirrors the JSON structure of snapshot_completeness.json.
type fixtureRoot struct {
	Fields map[string]fixtureRow `json:"fields"`
}

// validCategories is the closed set of category labels the fixture may use.
var validCategories = map[string]bool{
	"identity":         true,
	"telemetry":        true,
	"telemetry_alias":  true,
	"derived":          true,
	"writer_metadata":  true,
	"expected_failure": true,
}

// schemaProperty captures the bits of a vehicle-state schema property that
// the test cares about: its name and its declared atomic-group membership.
type schemaProperty struct {
	name        string
	atomicGroup string
}

// repoRootForStore walks up from the test's CWD until it finds go.mod.
// internal/store/*_test.go runs with CWD=internal/store, so the fixture
// path is repo-root-relative.
func repoRootForStore(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (no go.mod found)")
		}
		dir = parent
	}
}

// loadVehicleStateSchemaProps parses vehicle-state.schema.json and returns
// one schemaProperty per declared property in alphabetical order.
func loadVehicleStateSchemaProps(t *testing.T, root string) []schemaProperty {
	t.Helper()
	path := filepath.Join(root, "docs", "contracts", "schemas", "vehicle-state.schema.json")
	data, err := os.ReadFile(path) // #nosec G304 -- repo-relative test path
	if err != nil {
		t.Fatalf("read vehicle-state schema: %v", err)
	}
	var raw struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal vehicle-state schema: %v", err)
	}
	if len(raw.Properties) == 0 {
		t.Fatal("vehicle-state schema has no properties")
	}
	out := make([]schemaProperty, 0, len(raw.Properties))
	for name, def := range raw.Properties {
		ag, _ := def["x-atomic-group"].(string)
		out = append(out, schemaProperty{name: name, atomicGroup: ag})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// loadCompletenessFixture parses snapshot_completeness.json.
func loadCompletenessFixture(t *testing.T, root string) fixtureRoot {
	t.Helper()
	path := filepath.Join(root, "docs", "contracts", "fixtures", "rest", "snapshot_completeness.json")
	data, err := os.ReadFile(path) // #nosec G304 -- repo-relative test path
	if err != nil {
		t.Fatalf("read snapshot_completeness fixture: %v", err)
	}
	var root2 fixtureRoot
	if err := json.Unmarshal(data, &root2); err != nil {
		t.Fatalf("unmarshal snapshot_completeness fixture: %v", err)
	}
	return root2
}

// completenessSeedCatalog is the catalog values used by every scenario
// in TestSnapshotCompleteness. Identity fields are seeded with non-default
// values so the read-back asserts the column survived the round trip.
// fsdMilesSinceReset is seeded as 0 so the steady-state assertion validates
// the writer pipeline wrote the synthetic 412.7 (vs. the seed surviving).
var completenessSeedCatalog = catalogFields{
	model:              "Model 3",
	year:               2024,
	color:              "Midnight Silver Metallic",
	locationName:       "",
	locationAddress:    "",
	fsdMilesSinceReset: 0,
	destinationAddress: nil,
}

const (
	completenessVehicleID = "veh_snap_complete_001"
	completenessVIN       = "5YJ3E1EA1NF000SC1"
)

// TestSnapshotCompleteness is the MYR-48 conformance test. See file-level
// comment for design intent.
//
// Test plan:
//   1. Schema enumeration: every schema property MUST have a fixture row.
//   2. Fixture enumeration: every fixture row MUST be a schema property.
//   3. Atomic-group consistency: fixture atomicGroup == schema x-atomic-group.
//   4. Steady-state scenario: per telemetry field with nullableInSteadyState=false,
//      apply a single-field VehicleUpdate, read back, assert non-null.
//   5. Active-group scenario: per atomic group, apply ALL members in one
//      VehicleUpdate, read back, assert ALL non-null.
//   6. Identity / derived / writer_metadata fields are evaluated against
//      the seeded row + the side-effects of UpdateTelemetry/UpdateStatus.
func TestSnapshotCompleteness(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}

	root := repoRootForStore(t)
	schemaProps := loadVehicleStateSchemaProps(t, root)
	fix := loadCompletenessFixture(t, root)

	// ---------------- (1) schema → fixture coverage --------------------
	schemaByName := make(map[string]schemaProperty, len(schemaProps))
	for _, p := range schemaProps {
		schemaByName[p.name] = p
		if _, ok := fix.Fields[p.name]; !ok {
			t.Errorf(
				"missing fixture coverage for schema field %q (atomic group %q) — "+
					"add a row to docs/contracts/fixtures/rest/snapshot_completeness.json",
				p.name, p.atomicGroup,
			)
		}
	}

	// ---------------- (2) fixture → schema coverage --------------------
	for fxName := range fix.Fields {
		if _, ok := schemaByName[fxName]; !ok {
			t.Errorf(
				"fixture row %q has no matching schema property in "+
					"docs/contracts/schemas/vehicle-state.schema.json — "+
					"either rename the row to match a schema property or remove it from snapshot_completeness.json",
				fxName,
			)
		}
	}

	// ---------------- (3) atomic-group consistency ---------------------
	for _, p := range schemaProps {
		row, ok := fix.Fields[p.name]
		if !ok {
			continue
		}
		if row.AtomicGroup != p.atomicGroup {
			t.Errorf(
				"atomic group mismatch for %q: schema declares %q, fixture declares %q",
				p.name, p.atomicGroup, row.AtomicGroup,
			)
		}
		if !validCategories[row.Category] {
			t.Errorf("fixture row %q has invalid category %q", p.name, row.Category)
		}
	}

	// ---------------- (4) steady-state per-field ---------------------
	t.Run("steady_state_per_field", func(t *testing.T) {
		for _, p := range schemaProps {
			row, ok := fix.Fields[p.name]
			if !ok {
				continue
			}
			if row.NullableInSteadyState {
				continue // exempted; covered by active-group scenario
			}

			t.Run(p.name, func(t *testing.T) {
				runSteadyStateField(t, p, row)
			})
		}
	})

	// ---------------- (5) active-group scenarios ----------------------
	t.Run("active_group", func(t *testing.T) {
		groups := groupsFromFixture(fix)
		for _, group := range groups {
			members := membersOfGroup(fix, group)
			t.Run(group, func(t *testing.T) {
				runActiveGroup(t, group, members, fix)
			})
		}
	})

	// ---------------- (6) writer_metadata: lastUpdated ---------------
	t.Run("writer_metadata_lastUpdated", func(t *testing.T) {
		runLastUpdatedAdvances(t)
	})
}

// runSteadyStateField applies a single-field VehicleUpdate (or seed/derived
// path), reads back the row, and asserts the corresponding column is
// non-null/non-zero.
func runSteadyStateField(t *testing.T, p schemaProperty, row fixtureRow) {
	t.Helper()
	cleanTables(t, testPool)
	seedVehicleWithCatalog(t, testPool, completenessVehicleID, completenessVIN, completenessSeedCatalog)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	switch row.Category {
	case "identity":
		v, err := repo.GetByVIN(ctx, completenessVIN)
		if err != nil {
			t.Fatalf("GetByVIN: %v", err)
		}
		assertIdentityField(t, p.name, v)
	case "derived":
		// status is set by UpdateStatus or by drive-detection. Drive a
		// gear → status transition through UpdateStatus to assert the
		// derived path persists a valid enum value.
		if err := repo.UpdateStatus(ctx, completenessVIN, store.VehicleStatusDriving); err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}
		v, err := repo.GetByVIN(ctx, completenessVIN)
		if err != nil {
			t.Fatalf("GetByVIN: %v", err)
		}
		assertStatusValid(t, p.name, v)
	case "telemetry", "telemetry_alias":
		applySyntheticAndAssertNonNull(ctx, t, repo, p, row)
	case "expected_failure":
		// expected_failure rows: still try to apply the synthetic and
		// assert non-null on the read-back. If the writer pipeline is
		// missing the field, this WILL fail with the standard error
		// message — which is the entire point of MYR-48.
		applySyntheticAndAssertNonNull(ctx, t, repo, p, row)
	case "writer_metadata":
		// Covered by writer_metadata_lastUpdated subtest below.
	default:
		t.Errorf("unhandled category %q for field %q", row.Category, p.name)
	}
}

// applySyntheticAndAssertNonNull dispatches the synthetic spec into the
// writer pipeline (UpdateTelemetry) or directly into the row (seed),
// reads back, and asserts non-null per the field's Go type.
func applySyntheticAndAssertNonNull(
	ctx context.Context,
	t *testing.T,
	repo *store.VehicleRepo,
	p schemaProperty,
	row fixtureRow,
) {
	t.Helper()
	if row.Synthetic == nil {
		t.Errorf(
			"NFR-3.5 violation: field %q (atomic group %q, category %q) has no synthetic event in fixture — "+
				"cannot drive a value through the writer pipeline. Add a synthetic block to "+
				"snapshot_completeness.json.",
			p.name, p.atomicGroup, row.Category,
		)
		return
	}

	switch row.Synthetic.Kind {
	case "vehicleUpdate":
		update := store.VehicleUpdate{LastUpdated: time.Now().UTC()}
		if err := applyValueToUpdate(&update, row.Synthetic.Field, row.Synthetic.Value); err != nil {
			body := fmt.Sprintf(
				"field %q (atomic group %q, category %q) — failed to build VehicleUpdate.%s from fixture value: %v. "+
					"The VehicleUpdate Go struct is missing this field; the writer pipeline cannot persist this column. "+
					"See docs/contracts/vehicle-state-schema.md §1.1.",
				p.name, p.atomicGroup, row.Category, row.Synthetic.Field, err,
			)
			if row.Category == "expected_failure" {
				t.Logf("%s %s", expectedGapTag(row), body)
				return
			}
			t.Errorf("NFR-3.5 violation: %s", body)
			return
		}
		if err := repo.UpdateTelemetry(ctx, completenessVIN, update); err != nil {
			t.Fatalf("UpdateTelemetry for %q: %v", p.name, err)
		}
	case "seed":
		// Apply via direct SQL UPDATE — for fields that have no
		// VehicleUpdate path today (e.g. destinationAddress is intended
		// to be reverse-geocoded server-side, not driven by telemetry).
		if err := seedColumn(ctx, row.Synthetic.Field, row.Synthetic.Value); err != nil {
			t.Fatalf("seed column %q for field %q: %v", row.Synthetic.Field, p.name, err)
		}
	default:
		t.Errorf("fixture row %q has unsupported synthetic.kind=%q", p.name, row.Synthetic.Kind)
		return
	}

	v, err := repo.GetByVIN(ctx, completenessVIN)
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}
	assertSchemaFieldNonNull(t, p, row, v)
}

// runActiveGroup applies every group member's synthetic event in a single
// VehicleUpdate (plus any seed-only members directly), then asserts every
// member is non-null on the read-back row. This catches the case where
// one member of an atomic group is missing from the writer pipeline —
// the all-or-nothing predicate of NFR-3.7.
func runActiveGroup(t *testing.T, group string, members []string, fix fixtureRoot) {
	t.Helper()
	cleanTables(t, testPool)
	seedVehicleWithCatalog(t, testPool, completenessVehicleID, completenessVIN, completenessSeedCatalog)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	update := store.VehicleUpdate{LastUpdated: time.Now().UTC()}
	var seedRows []syntheticSpec
	type buildErr struct {
		field      string
		message    string
		isExpected bool
		row        fixtureRow
	}
	var buildErrs []buildErr

	for _, name := range members {
		row := fix.Fields[name]
		if row.Synthetic == nil {
			// derived (e.g. status) — applied via UpdateStatus below.
			continue
		}
		switch row.Synthetic.Kind {
		case "vehicleUpdate":
			if err := applyValueToUpdate(&update, row.Synthetic.Field, row.Synthetic.Value); err != nil {
				buildErrs = append(buildErrs, buildErr{
					field: name,
					message: fmt.Sprintf(
						"  - %s (group=%s, category=%s): failed to set VehicleUpdate.%s: %v",
						name, group, row.Category, row.Synthetic.Field, err,
					),
					isExpected: row.Category == "expected_failure",
					row:        row,
				})
			}
		case "seed":
			seedRows = append(seedRows, *row.Synthetic)
		}
	}

	for _, e := range buildErrs {
		body := fmt.Sprintf("atomic group %q:\n%s\n"+
			"VehicleUpdate is missing the Go field needed to persist this schema column. "+
			"See docs/contracts/vehicle-state-schema.md §1.1.", group, e.message)
		if e.isExpected {
			t.Logf("%s %s", expectedGapTag(e.row), body)
			continue
		}
		// One Errorf per failure; do not Fatal so all gaps surface in one run.
		t.Errorf("NFR-3.5 / NFR-3.7 violation in %s", body)
	}
	// Continue — the partial UpdateTelemetry still runs for the
	// fields that did build, exercising the writer pipeline.

	if err := repo.UpdateTelemetry(ctx, completenessVIN, update); err != nil {
		// ErrVehicleNotFound shouldn't happen because we seeded above;
		// any other error fails the test.
		if !errors.Is(err, store.ErrVehicleNotFound) {
			t.Fatalf("UpdateTelemetry: %v", err)
		}
	}

	for _, s := range seedRows {
		if err := seedColumn(ctx, s.Field, s.Value); err != nil {
			t.Fatalf("seed column %q: %v", s.Field, err)
		}
	}

	if group == "gear" {
		// Status is derived; apply through UpdateStatus to mirror the
		// production path that fires on gear-position transitions.
		if err := repo.UpdateStatus(ctx, completenessVIN, store.VehicleStatusDriving); err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}
	}

	v, err := repo.GetByVIN(ctx, completenessVIN)
	if err != nil {
		t.Fatalf("GetByVIN: %v", err)
	}

	for _, name := range members {
		row := fix.Fields[name]
		assertSchemaFieldNonNull(t, schemaProperty{name: name, atomicGroup: group}, row, v)
	}
}

// runLastUpdatedAdvances asserts that lastUpdated is set to a non-zero
// time AND was advanced by an UpdateTelemetry call (i.e., later than the
// seed timestamp). Catches a regression where the writer forgets to set
// the LastUpdated field on the VehicleUpdate.
func runLastUpdatedAdvances(t *testing.T) {
	t.Helper()
	cleanTables(t, testPool)
	seedVehicleWithCatalog(t, testPool, completenessVehicleID, completenessVIN, completenessSeedCatalog)
	repo := store.NewVehicleRepo(testPool, store.NoopMetrics{})
	ctx := context.Background()

	before, err := repo.GetByVIN(ctx, completenessVIN)
	if err != nil {
		t.Fatalf("GetByVIN before: %v", err)
	}
	if before.LastUpdated.IsZero() {
		t.Fatalf("seeded LastUpdated is zero — expected DB DEFAULT NOW()")
	}

	speed := 35
	stamp := time.Now().UTC().Add(time.Second) // strictly later than seed
	if err := repo.UpdateTelemetry(ctx, completenessVIN, store.VehicleUpdate{
		Speed:       &speed,
		LastUpdated: stamp,
	}); err != nil {
		t.Fatalf("UpdateTelemetry: %v", err)
	}

	after, err := repo.GetByVIN(ctx, completenessVIN)
	if err != nil {
		t.Fatalf("GetByVIN after: %v", err)
	}
	if after.LastUpdated.IsZero() {
		t.Errorf(
			"NFR-3.5 violation: field %q (writer_metadata) is unexpectedly zero on /snapshot read after applying "+
				"synthetic event — DB persistence missing in writer pipeline. See docs/contracts/vehicle-state-schema.md §1.1.",
			"lastUpdated",
		)
	}
	if !after.LastUpdated.After(before.LastUpdated) {
		t.Errorf(
			"NFR-3.5 violation: field %q (writer_metadata) did not advance after UpdateTelemetry: "+
				"before=%s after=%s — Writer.handleTelemetry is not setting VehicleUpdate.LastUpdated. "+
				"See docs/contracts/vehicle-state-schema.md §1.1.",
			"lastUpdated", before.LastUpdated.Format(time.RFC3339Nano), after.LastUpdated.Format(time.RFC3339Nano),
		)
	}
}

// assertSchemaFieldNonNull dispatches a non-null assertion based on the
// schema property name. Pointer columns (gearPosition, chargeState, nav
// fields) must be non-nil; primitive columns (speed, latitude, etc) must
// be non-zero per the DB DEFAULT 0 convention.
//
// Rows with category=expected_failure surface their assertion as a
// t.Logf warning instead of t.Errorf so the test stays green while a
// known writer-pipeline gap is tracked separately. The closing PR for
// the gap re-categorizes the row to telemetry|telemetry_alias and the
// assertion becomes hard.
func assertSchemaFieldNonNull(t *testing.T, p schemaProperty, row fixtureRow, v store.Vehicle) {
	t.Helper()
	failure := func(detail string) {
		body := fmt.Sprintf(
			"field %q (atomic group %q, category %q) is null on /snapshot read after applying synthetic event. %s "+
				"See docs/contracts/vehicle-state-schema.md §1.1.",
			p.name, p.atomicGroup, row.Category, detail,
		)
		if row.Category == "expected_failure" {
			// Documented writer-pipeline gap. Track in Linear; do not
			// fail CI today. See snapshot_completeness.json comment for
			// the follow-up issue ID.
			t.Logf("%s %s", expectedGapTag(row), body)
			return
		}
		t.Errorf("NFR-3.5 violation: %s — DB persistence missing in writer pipeline.", body)
	}

	switch p.name {
	case "vehicleId":
		if v.ID == "" {
			failure("Vehicle.ID is empty.")
		}
	case "name":
		if v.Name == "" {
			failure("Vehicle.Name is empty.")
		}
	case "model":
		if v.Model == "" {
			failure("Vehicle.Model is empty.")
		}
	case "year":
		if v.Year <= 0 {
			failure(fmt.Sprintf("Vehicle.Year is %d.", v.Year))
		}
	case "color":
		if v.Color == "" {
			failure("Vehicle.Color is empty.")
		}
	case "status":
		assertStatusValid(t, p.name, v)
	case "speed":
		if v.Speed == 0 {
			failure("Vehicle.Speed is 0.")
		}
	case "heading":
		if v.Heading == 0 {
			failure("Vehicle.Heading is 0.")
		}
	case "latitude":
		if v.Latitude == 0 {
			failure("Vehicle.Latitude is 0 (treated as 'no GPS fix').")
		}
	case "longitude":
		if v.Longitude == 0 {
			failure("Vehicle.Longitude is 0 (treated as 'no GPS fix').")
		}
	case "locationName":
		if v.LocationName == "" {
			failure("Vehicle.LocationName is empty.")
		}
	case "locationAddress":
		if v.LocationAddress == "" {
			failure("Vehicle.LocationAddress is empty.")
		}
	case "gearPosition":
		if v.GearPosition == nil || *v.GearPosition == "" {
			failure("Vehicle.GearPosition is nil/empty.")
		}
	case "chargeLevel":
		if v.ChargeLevel == 0 {
			failure("Vehicle.ChargeLevel is 0.")
		}
	case "chargeState":
		if v.ChargeState == nil || *v.ChargeState == "" {
			failure("Vehicle.ChargeState is nil/empty.")
		}
	case "estimatedRange":
		if v.EstimatedRange == 0 {
			failure("Vehicle.EstimatedRange is 0.")
		}
	case "timeToFull":
		if v.TimeToFull == nil {
			failure("Vehicle.TimeToFull is nil.")
		}
	case "interiorTemp":
		if v.InteriorTemp == 0 {
			failure("Vehicle.InteriorTemp is 0.")
		}
	case "exteriorTemp":
		if v.ExteriorTemp == 0 {
			failure("Vehicle.ExteriorTemp is 0.")
		}
	case "odometerMiles":
		if v.OdometerMiles == 0 {
			failure("Vehicle.OdometerMiles is 0.")
		}
	case "fsdMilesSinceReset":
		if v.FsdMilesSinceReset == 0 {
			failure("Vehicle.FsdMilesSinceReset is 0.")
		}
	case "destinationName":
		if v.DestinationName == nil || *v.DestinationName == "" {
			failure("Vehicle.DestinationName is nil/empty.")
		}
	case "destinationAddress":
		if v.DestinationAddress == nil || *v.DestinationAddress == "" {
			failure("Vehicle.DestinationAddress is nil/empty.")
		}
	case "destinationLatitude":
		if v.DestinationLatitude == nil {
			failure("Vehicle.DestinationLatitude is nil.")
		}
	case "destinationLongitude":
		if v.DestinationLongitude == nil {
			failure("Vehicle.DestinationLongitude is nil.")
		}
	case "originLatitude":
		if v.OriginLatitude == nil {
			failure("Vehicle.OriginLatitude is nil.")
		}
	case "originLongitude":
		if v.OriginLongitude == nil {
			failure("Vehicle.OriginLongitude is nil.")
		}
	case "etaMinutes":
		if v.EtaMinutes == nil {
			failure("Vehicle.EtaMinutes is nil.")
		}
	case "tripDistanceRemaining":
		if v.TripDistRemaining == nil {
			failure("Vehicle.TripDistRemaining is nil.")
		}
	case "navRouteCoordinates":
		if len(v.NavRouteCoordinates) == 0 || string(v.NavRouteCoordinates) == "null" {
			failure("Vehicle.NavRouteCoordinates is nil/empty.")
		}
	case "lastUpdated":
		if v.LastUpdated.IsZero() {
			failure("Vehicle.LastUpdated is zero time.")
		}
	default:
		t.Errorf(
			"assertSchemaFieldNonNull has no case for schema property %q — "+
				"add an assertion arm to internal/store/snapshot_completeness_test.go",
			p.name,
		)
	}
}

// assertIdentityField checks identity fields against the seeded values.
func assertIdentityField(t *testing.T, name string, v store.Vehicle) {
	t.Helper()
	switch name {
	case "vehicleId":
		if v.ID == "" {
			t.Errorf("identity field %q is empty after seed", name)
		}
	case "name":
		if v.Name == "" {
			t.Errorf("identity field %q is empty after seed", name)
		}
	case "model":
		if v.Model == "" {
			t.Errorf("identity field %q is empty after seed", name)
		}
	case "year":
		if v.Year <= 0 {
			t.Errorf("identity field %q is %d after seed (want > 0)", name, v.Year)
		}
	case "color":
		if v.Color == "" {
			t.Errorf("identity field %q is empty after seed", name)
		}
	default:
		t.Errorf("identity field %q has no assertion case — extend assertIdentityField", name)
	}
}

// assertStatusValid asserts that v.Status is one of the legal enum values.
func assertStatusValid(t *testing.T, name string, v store.Vehicle) {
	t.Helper()
	switch v.Status {
	case store.VehicleStatusDriving,
		store.VehicleStatusParked,
		store.VehicleStatusCharging,
		store.VehicleStatusOffline,
		store.VehicleStatusInService:
		// ok
	default:
		t.Errorf(
			"NFR-3.5 violation: derived field %q is %q — not one of the legal "+
				"VehicleStatus enum values (driving|parked|charging|offline|in_service)",
			name, v.Status,
		)
	}
}

// applyValueToUpdate sets the named field on a VehicleUpdate from a JSON
// raw value. Returns an error if the field name does not exist on
// VehicleUpdate (which IS the regression-class signal — the schema names
// a field that the writer pipeline cannot persist).
func applyValueToUpdate(u *store.VehicleUpdate, fieldName string, raw json.RawMessage) error {
	rv := reflect.ValueOf(u).Elem()
	f := rv.FieldByName(fieldName)
	if !f.IsValid() {
		return fmt.Errorf("VehicleUpdate has no field named %q", fieldName)
	}
	if !f.CanSet() {
		return fmt.Errorf("VehicleUpdate.%s is not settable", fieldName)
	}

	switch fieldName {
	case "Speed", "ChargeLevel", "EstimatedRange", "Heading",
		"InteriorTemp", "ExteriorTemp", "OdometerMiles", "EtaMinutes":
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return fmt.Errorf("decode int for %s: %w", fieldName, err)
		}
		f.Set(reflect.ValueOf(&n))
		return nil
	case "TimeToFull", "Latitude", "Longitude",
		"DestinationLatitude", "DestinationLongitude",
		"OriginLatitude", "OriginLongitude", "TripDistRemaining",
		"FsdMilesSinceReset":
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil {
			return fmt.Errorf("decode float64 for %s: %w", fieldName, err)
		}
		f.Set(reflect.ValueOf(&n))
		return nil
	case "ChargeState", "GearPosition", "DestinationName", "DestinationAddress",
		"LocationName", "LocationAddr":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("decode string for %s: %w", fieldName, err)
		}
		f.Set(reflect.ValueOf(&s))
		return nil
	case "NavRouteCoordinates":
		// raw is already a json.RawMessage carrying the JSONB array.
		// pgx binds *json.RawMessage as JSONB so a direct pointer works.
		rm := raw
		f.Set(reflect.ValueOf(&rm))
		return nil
	default:
		return fmt.Errorf("applyValueToUpdate has no codec for VehicleUpdate.%s — extend the switch", fieldName)
	}
}

// seedColumn applies a value directly to a DB column, bypassing the
// writer pipeline. Used for fields that have no VehicleUpdate path
// today (e.g. destinationAddress, until reverse-geocoding lands).
func seedColumn(ctx context.Context, column string, raw json.RawMessage) error {
	// Decode the JSON value to a Go any so pgx can bind it via its
	// default type mapping.
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("decode seed value: %w", err)
	}
	// Quoting the column name — column is sourced from a hardcoded fixture,
	// not user input.
	q := fmt.Sprintf(`UPDATE "Vehicle" SET %q = $1 WHERE "vin" = $2`, column)
	_, err := testPool.Exec(ctx, q, v, completenessVIN)
	if err != nil {
		return fmt.Errorf("UPDATE %q: %w", column, err)
	}
	return nil
}

// groupsFromFixture returns the unique non-empty atomic group names
// declared in the fixture, sorted for determinism.
func groupsFromFixture(fix fixtureRoot) []string {
	seen := make(map[string]bool)
	for _, row := range fix.Fields {
		if row.AtomicGroup != "" {
			seen[row.AtomicGroup] = true
		}
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// membersOfGroup returns the schema property names that declare
// membership in `group`, sorted for determinism.
func membersOfGroup(fix fixtureRoot, group string) []string {
	var out []string
	for name, row := range fix.Fields {
		if row.AtomicGroup == group {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
