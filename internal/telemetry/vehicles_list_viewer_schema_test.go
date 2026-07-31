package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// Schema conformance for the VIEWER-masked VehicleSummary row.
//
// This exists because of a defect class the structural tests could not see: the
// viewer mask stripped `name`, which vehicle-summary.schema.json lists under
// `required`, so EVERY viewer row on GET /api/vehicles and every row of the
// redeem response was schema-invalid while each individual field assertion
// passed. The fix is to validate the ACTUAL projection against the ACTUAL
// schema file, so any future mask subtraction of a required field fails here
// rather than in a client's decoder.
//
// viewerSummaryMap is the single projection both surfaces go through
// (vehicles_list_viewer.go for the list merge, share_redeem_handler.go for the
// join screen), so validating it covers both.

// vehicleSummarySchemaID is the per-row $defs pointer inside the list-envelope
// schema file. The file's ROOT is VehicleListResponse; a single row lives under
// $defs, so this is the URI that validates one item.
const vehicleSummarySchemaID = "https://myrobotaxi.com/schemas/vehicle-summary.schema.json#/$defs/VehicleSummary"

func TestViewerSummaryRowMatchesVehicleSummarySchema(t *testing.T) {
	schema := compileContractSchema(t, vehicleSummarySchemaID)

	// MYR-369: the two grant shapes the product now has. A SUSPENDED grant is
	// deliberately absent — it produces no viewer row at all, so there is no
	// projection of one to validate.
	grants := map[string]auth.ShareGrant{
		"base":  {},
		"rides": {AllowRides: true},
	}
	for name, grant := range grants {
		t.Run(name, func(t *testing.T) {
			row := sharedCatalogRow(grant.AllowRides)
			projected := viewerSummaryMap(row.VehicleCatalogRow, grant)

			// Serialize and re-parse through the schema library's decoder so
			// what is validated is the BYTES a client receives, not the Go map.
			body, err := json.Marshal(projected)
			if err != nil {
				t.Fatalf("marshal viewer row: %v", err)
			}
			parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("re-parse viewer row: %v", err)
			}
			if err := schema.Validate(parsed); err != nil {
				t.Fatalf("viewer-masked row does not satisfy VehicleSummary:\n%v\nbody: %s", err, body)
			}
		})
	}
}

// TestViewerMaskKeepsEverySchemaRequiredField walks the schema's own `required`
// list and asserts the viewer projection carries each entry.
//
// Deliberately driven off the SCHEMA rather than a hand-copied list: a field
// promoted to required in contracts must either survive the viewer mask or be
// argued about here, which is exactly the conversation that did not happen when
// `name` was stripped.
func TestViewerMaskKeepsEverySchemaRequiredField(t *testing.T) {
	required := schemaRequiredFields(t, "vehicle-summary.schema.json", "VehicleSummary")
	if len(required) == 0 {
		t.Fatal("read zero required fields from vehicle-summary.schema.json — the walk is broken")
	}

	grant := auth.ShareGrant{AllowRides: true}
	row := sharedCatalogRow(grant.AllowRides)
	projected := viewerSummaryMap(row.VehicleCatalogRow, grant)

	for _, field := range required {
		if _, ok := projected[field]; !ok {
			t.Errorf("viewer row omits %q, which VehicleSummary marks required — "+
				"either keep it in the viewer allow-list or make it optional in the schema", field)
		}
	}
}

// ---------------------------------------------------------------------------
// Schema helpers
// ---------------------------------------------------------------------------

// contractSchemasDir is the canonical schema directory, relative to repo root.
const contractSchemasDir = "docs/contracts/schemas"

// compileContractSchema builds a compiler pre-loaded with EVERY schema in the
// contracts directory (cross-file $refs — VehicleSummary.sharePermission points
// into vehicle-sharing.schema.json — do not resolve otherwise) and compiles the
// requested URI.
func compileContractSchema(t *testing.T, id string) *jsonschema.Schema {
	t.Helper()

	c := jsonschema.NewCompiler()
	for _, path := range contractSchemaFiles(t) {
		raw := readSchemaResource(t, path)
		schemaID, _ := raw["$id"].(string)
		if schemaID == "" {
			schemaID = "file:///" + filepath.Base(path)
		}
		if err := c.AddResource(schemaID, raw); err != nil {
			t.Fatalf("add schema resource %s: %v", path, err)
		}
	}

	schema, err := c.Compile(id)
	if err != nil {
		t.Fatalf("compile %s: %v", id, err)
	}
	return schema
}

// schemaRequiredFields returns the `required` list of a named $defs entry.
func schemaRequiredFields(t *testing.T, file, def string) []string {
	t.Helper()

	raw := readSchemaResource(t, filepath.Join(contractsSchemaRoot(t), file))
	defs, ok := raw["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no $defs object", file)
	}
	target, ok := defs[def].(map[string]any)
	if !ok {
		t.Fatalf("%s has no $defs/%s object", file, def)
	}
	rawRequired, ok := target["required"].([]any)
	if !ok {
		t.Fatalf("%s $defs/%s has no required array", file, def)
	}

	out := make([]string, 0, len(rawRequired))
	for _, r := range rawRequired {
		name, ok := r.(string)
		if !ok {
			t.Fatalf("%s $defs/%s: non-string entry in required", file, def)
		}
		out = append(out, name)
	}
	return out
}

// readSchemaResource parses one schema file with the json.Number-preserving
// decoder the compiler expects.
func readSchemaResource(t *testing.T, path string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test-only read of a repo-relative contract schema
	if err != nil {
		t.Fatalf("read schema %s: %v", path, err)
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unmarshal schema %s: %v", path, err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		t.Fatalf("schema %s is not a JSON object", path)
	}
	return m
}

// contractSchemaFiles lists every *.json under the contracts schema directory.
func contractSchemaFiles(t *testing.T) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(contractsSchemaRoot(t), "*.json"))
	if err != nil {
		t.Fatalf("glob schemas: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no schema files under %s", contractsSchemaRoot(t))
	}
	return files
}

// contractsSchemaRoot resolves the schema directory from the repo root, found
// by walking up to the go.mod. The package's own working directory is
// internal/telemetry, so a relative path alone would not survive a move.
func contractsSchemaRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, contractSchemasDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (no go.mod found)")
		}
		dir = parent
	}
}
