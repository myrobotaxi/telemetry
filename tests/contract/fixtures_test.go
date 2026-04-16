// Package contract_test validates every canonical JSON fixture against its
// corresponding JSON Schema. This is Layer 1 (contract conformance) of the
// test bench defined in docs/architecture/requirements.md §3.15.
//
// The fixtures in docs/contracts/fixtures/ are the source of truth for message
// shapes. If a fixture fails validation, the fixture or the schema is wrong —
// never silently skip.
package contract_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ---------------------------------------------------------------------------
// Paths (relative to repo root)
// ---------------------------------------------------------------------------

const (
	fixturesDir = "docs/contracts/fixtures"
	schemasDir  = "docs/contracts/schemas"
)

// repoRoot walks up from the test file's directory until it finds go.mod.
func repoRoot(t *testing.T) string {
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

// ---------------------------------------------------------------------------
// Schema compiler helpers
// ---------------------------------------------------------------------------

// newCompiler returns a jsonschema compiler pre-loaded with all schemas in the
// schemas/ directory so that $ref resolution works across files.
func newCompiler(t *testing.T, root string) *jsonschema.Compiler {
	t.Helper()

	c := jsonschema.NewCompiler()

	schemaFiles, err := filepath.Glob(filepath.Join(root, schemasDir, "*.json"))
	if err != nil {
		t.Fatalf("glob schemas: %v", err)
	}
	if len(schemaFiles) == 0 {
		t.Fatalf("no schema files found in %s", filepath.Join(root, schemasDir))
	}

	for _, sf := range schemaFiles {
		data, err := os.ReadFile(sf)
		if err != nil {
			t.Fatalf("read schema %s: %v", sf, err)
		}
		// Use jsonschema.UnmarshalJSON to get json.Number types for proper
		// schema compilation (the library expects UseNumber-decoded values).
		raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("unmarshal schema %s: %v", sf, err)
		}
		// Use the $id from the schema as the URI.
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("schema %s is not an object", sf)
		}
		id, _ := m["$id"].(string)
		if id == "" {
			// Fallback: use filename as URI.
			id = "file:///" + filepath.Base(sf)
		}
		if err := c.AddResource(id, raw); err != nil {
			t.Fatalf("add schema resource %s (%s): %v", sf, id, err)
		}
	}
	return c
}

// compileSchema compiles a schema by its $id URI.
func compileSchema(t *testing.T, c *jsonschema.Compiler, id string) *jsonschema.Schema {
	t.Helper()
	s, err := c.Compile(id)
	if err != nil {
		t.Fatalf("compile schema %s: %v", id, err)
	}
	return s
}

// compileDef compiles a $defs sub-schema from ws-messages.schema.json.
func compileDef(t *testing.T, c *jsonschema.Compiler, defName string) *jsonschema.Schema {
	t.Helper()
	uri := "https://myrobotaxi.com/schemas/ws-messages.schema.json#/$defs/" + defName
	s, err := c.Compile(uri)
	if err != nil {
		t.Fatalf("compile $defs/%s: %v", defName, err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Fixture loading helpers
// ---------------------------------------------------------------------------

// loadFixture reads a fixture file and returns its parsed JSON object. Uses
// jsonschema.UnmarshalJSON (which enables json.Number) so that integers are
// preserved for JSON Schema validation rather than being widened to float64.
func loadFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unmarshal fixture %s: %v", path, err)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("fixture %s is not a JSON object", path)
	}
	return m
}

// stripMeta returns a copy of m without the "_meta" key.
func stripMeta(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "_meta" {
			continue
		}
		out[k] = v
	}
	return out
}

// validate checks v against schema s and fails the test with a descriptive
// message if validation fails.
func validate(t *testing.T, s *jsonschema.Schema, v any, context string) {
	t.Helper()
	err := s.Validate(v)
	if err != nil {
		t.Errorf("schema validation failed for %s:\n%v", context, err)
	}
}

// ---------------------------------------------------------------------------
// Test: All fixtures validate against schemas
// ---------------------------------------------------------------------------

func TestFixturesValidateAgainstSchemas(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)

	envelopeSchema := compileSchema(t, c, "https://myrobotaxi.com/schemas/ws-envelope.schema.json")
	vehicleStateSchema := compileSchema(t, c, "https://myrobotaxi.com/schemas/vehicle-state.schema.json")

	// Pre-compile all WS message payload schemas.
	payloadSchemas := map[string]*jsonschema.Schema{
		"auth":         compileDef(t, c, "AuthPayload"),
		"auth_ok":      compileDef(t, c, "AuthOkPayload"),
		"vehicle_update": compileDef(t, c, "VehicleUpdatePayload"),
		"drive_started": compileDef(t, c, "DriveStartedPayload"),
		"drive_ended":   compileDef(t, c, "DriveEndedPayload"),
		"connectivity":  compileDef(t, c, "ConnectivityPayload"),
		"error":         compileDef(t, c, "ErrorPayload"),
	}

	// Build a table of all fixture files and their expected validation.
	type testCase struct {
		name     string
		path     string
		validate func(t *testing.T, m map[string]any)
	}

	var cases []testCase

	fixturesRoot := filepath.Join(root, fixturesDir)

	// -----------------------------------------------------------------------
	// websocket/ fixtures
	// -----------------------------------------------------------------------
	wsDir := filepath.Join(fixturesRoot, "websocket")
	wsFiles := mustGlobJSON(t, wsDir)
	for _, f := range wsFiles {
		f := f // capture
		baseName := filepath.Base(f)
		cases = append(cases, testCase{
			name: "websocket/" + baseName,
			path: f,
			validate: func(t *testing.T, m map[string]any) {
				stripped := stripMeta(m)

				// Every WS fixture must validate against the envelope schema.
				validate(t, envelopeSchema, stripped, "envelope")

				// Determine the message type.
				msgType, ok := stripped["type"].(string)
				if !ok {
					t.Fatalf("fixture missing 'type' string field")
				}

				// heartbeat has no payload — skip payload validation.
				if msgType == "heartbeat" {
					if _, hasPayload := stripped["payload"]; hasPayload {
						t.Errorf("heartbeat fixture should not have a payload")
					}
					return
				}

				// All other types must have a payload.
				payload, hasPayload := stripped["payload"]
				if !hasPayload {
					t.Fatalf("fixture type=%s missing 'payload'", msgType)
				}

				// Map fixture filename prefix to payload schema key.
				schemaKey := wsSchemaKeyFromType(msgType)
				ps, ok := payloadSchemas[schemaKey]
				if !ok {
					t.Fatalf("no payload schema mapped for type=%s (key=%s)", msgType, schemaKey)
				}
				validate(t, ps, payload, fmt.Sprintf("payload ($defs/%s)", schemaKey))
			},
		})
	}

	// -----------------------------------------------------------------------
	// rest/ fixtures
	// -----------------------------------------------------------------------
	restDir := filepath.Join(fixturesRoot, "rest")
	restFiles := mustGlobJSON(t, restDir)
	for _, f := range restFiles {
		f := f
		baseName := filepath.Base(f)
		cases = append(cases, testCase{
			name: "rest/" + baseName,
			path: f,
			validate: func(t *testing.T, m map[string]any) {
				stripped := stripMeta(m)

				switch {
				case baseName == "snapshot.json":
					validate(t, vehicleStateSchema, stripped, "VehicleState snapshot")

				case baseName == "drives.json":
					validateDrivesList(t, stripped)

				case baseName == "drive_detail.json":
					validateDriveDetail(t, stripped)

				case baseName == "drive_route.json":
					validateDriveRoute(t, stripped)

				case strings.HasPrefix(baseName, "error."):
					validateRESTError(t, stripped)

				default:
					t.Errorf("unrecognized REST fixture: %s — add validation mapping", baseName)
				}
			},
		})
	}

	// -----------------------------------------------------------------------
	// edge-cases/ fixtures
	// -----------------------------------------------------------------------
	edgeDir := filepath.Join(fixturesRoot, "edge-cases")
	edgeFiles := mustGlobJSON(t, edgeDir)
	for _, f := range edgeFiles {
		f := f
		baseName := filepath.Base(f)
		cases = append(cases, testCase{
			name: "edge-cases/" + baseName,
			path: f,
			validate: func(t *testing.T, m map[string]any) {
				stripped := stripMeta(m)

				switch {
				case strings.HasPrefix(baseName, "snapshot."):
					validate(t, vehicleStateSchema, stripped, "VehicleState edge-case snapshot")

				case strings.HasPrefix(baseName, "vehicle_update."):
					validate(t, envelopeSchema, stripped, "envelope")
					if payload, ok := stripped["payload"]; ok {
						validate(t, payloadSchemas["vehicle_update"], payload, "VehicleUpdatePayload")
					} else {
						t.Fatalf("vehicle_update edge-case missing payload")
					}

				case strings.HasPrefix(baseName, "drive_ended."):
					validate(t, envelopeSchema, stripped, "envelope")
					if payload, ok := stripped["payload"]; ok {
						validate(t, payloadSchemas["drive_ended"], payload, "DriveEndedPayload")
					} else {
						t.Fatalf("drive_ended edge-case missing payload")
					}

				case strings.HasPrefix(baseName, "error."):
					validate(t, envelopeSchema, stripped, "envelope")
					if payload, ok := stripped["payload"]; ok {
						validate(t, payloadSchemas["error"], payload, "ErrorPayload")
					} else {
						t.Fatalf("error edge-case missing payload")
					}

				default:
					t.Errorf("unrecognized edge-case fixture: %s — add validation mapping", baseName)
				}
			},
		})
	}

	// -----------------------------------------------------------------------
	// Run all cases
	// -----------------------------------------------------------------------
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := loadFixture(t, tc.path)
			tc.validate(t, m)
		})
	}
}

// wsSchemaKeyFromType maps a WebSocket message type string to the payload
// schema lookup key used in TestFixturesValidateAgainstSchemas.
func wsSchemaKeyFromType(msgType string) string {
	// All types map 1:1 to the key except heartbeat (no payload, handled
	// separately). The type string IS the key.
	return msgType
}

// ---------------------------------------------------------------------------
// Test: Atomic group completeness
// ---------------------------------------------------------------------------

func TestAtomicGroupCompleteness(t *testing.T) {
	root := repoRoot(t)

	// Load x-atomic-groups from vehicle-state.schema.json.
	vsPath := filepath.Join(root, schemasDir, "vehicle-state.schema.json")
	vsData, err := os.ReadFile(vsPath)
	if err != nil {
		t.Fatalf("read vehicle-state schema: %v", err)
	}
	vsRaw, err := jsonschema.UnmarshalJSON(bytes.NewReader(vsData))
	if err != nil {
		t.Fatalf("unmarshal vehicle-state schema: %v", err)
	}
	vs, ok := vsRaw.(map[string]any)
	if !ok {
		t.Fatal("vehicle-state.schema.json is not a JSON object")
	}

	xAG, ok := vs["x-atomic-groups"].(map[string]any)
	if !ok {
		t.Fatal("vehicle-state.schema.json missing x-atomic-groups")
	}

	// For each declared atomic group, load the corresponding fixture and
	// verify its fields match exactly.
	agDir := filepath.Join(root, fixturesDir, "atomic-groups")

	for groupName, groupDef := range xAG {
		t.Run(groupName, func(t *testing.T) {
			gd, ok := groupDef.(map[string]any)
			if !ok {
				t.Fatalf("x-atomic-groups.%s is not an object", groupName)
			}
			declaredFieldsRaw, ok := gd["fields"].([]any)
			if !ok {
				t.Fatalf("x-atomic-groups.%s.fields is not an array", groupName)
			}
			declaredFields := make(map[string]bool, len(declaredFieldsRaw))
			for _, f := range declaredFieldsRaw {
				s, ok := f.(string)
				if !ok {
					t.Fatalf("x-atomic-groups.%s.fields contains non-string", groupName)
				}
				declaredFields[s] = true
			}

			// Load fixture.
			fixturePath := filepath.Join(agDir, groupName+".json")
			m := loadFixture(t, fixturePath)
			stripped := stripMeta(m)

			fieldsMap, ok := stripped["fields"].(map[string]any)
			if !ok {
				t.Fatalf("atomic-group fixture %s.json missing 'fields' object", groupName)
			}

			// Check fixture has exactly the declared fields.
			fixtureFields := make(map[string]bool, len(fieldsMap))
			for k := range fieldsMap {
				fixtureFields[k] = true
			}

			for df := range declaredFields {
				if !fixtureFields[df] {
					t.Errorf("declared field %q missing from fixture", df)
				}
			}
			for ff := range fixtureFields {
				if !declaredFields[ff] {
					t.Errorf("fixture field %q not declared in x-atomic-groups.%s", ff, groupName)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: Every fixture has a valid _meta block
// ---------------------------------------------------------------------------

func TestFixtureMetaBlocks(t *testing.T) {
	root := repoRoot(t)
	fixturesRoot := filepath.Join(root, fixturesDir)

	validScenarios := map[string]bool{
		"happy-path":   true,
		"edge-case":    true,
		"error":        true,
		"transitional": true,
	}

	allFixtures := mustGlobJSONRecursive(t, fixturesRoot)
	if len(allFixtures) == 0 {
		t.Fatal("no fixture files found")
	}

	for _, f := range allFixtures {
		relPath, _ := filepath.Rel(fixturesRoot, f)
		t.Run(relPath, func(t *testing.T) {
			m := loadFixture(t, f)

			metaRaw, ok := m["_meta"]
			if !ok {
				t.Fatal("fixture missing _meta block")
			}

			meta, ok := metaRaw.(map[string]any)
			if !ok {
				t.Fatal("_meta is not an object")
			}

			// Required: description (string)
			desc, ok := meta["description"].(string)
			if !ok || desc == "" {
				t.Error("_meta.description missing or empty")
			}

			// Required: anchoredFRs (non-empty array of strings)
			frs, ok := meta["anchoredFRs"].([]any)
			if !ok || len(frs) == 0 {
				t.Error("_meta.anchoredFRs missing or empty")
			}
			for i, fr := range frs {
				if _, ok := fr.(string); !ok {
					t.Errorf("_meta.anchoredFRs[%d] is not a string", i)
				}
			}

			// Required: scenario (valid enum)
			scenario, ok := meta["scenario"].(string)
			if !ok || scenario == "" {
				t.Error("_meta.scenario missing or empty")
			} else if !validScenarios[scenario] {
				t.Errorf("_meta.scenario %q is not one of: happy-path, edge-case, error, transitional", scenario)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// REST structural validation helpers
// ---------------------------------------------------------------------------

// validateDrivesList performs structural validation on the drives list fixture.
func validateDrivesList(t *testing.T, m map[string]any) {
	t.Helper()

	// Required top-level: items, nextCursor, hasMore
	items, ok := m["items"].([]any)
	if !ok {
		t.Error("drives fixture missing 'items' array")
		return
	}

	if _, ok := m["nextCursor"]; !ok {
		t.Error("drives fixture missing 'nextCursor'")
	}
	if _, ok := m["hasMore"]; !ok {
		t.Error("drives fixture missing 'hasMore'")
	}

	// Required DriveSummary fields per OpenAPI spec.
	driveSummaryRequired := []string{
		"id", "vehicleId", "startTime", "endTime", "date",
		"distanceMiles", "durationSeconds", "avgSpeedMph", "maxSpeedMph",
		"startChargeLevel", "endChargeLevel", "createdAt",
	}

	for i, item := range items {
		drive, ok := item.(map[string]any)
		if !ok {
			t.Errorf("items[%d] is not an object", i)
			continue
		}
		for _, field := range driveSummaryRequired {
			if _, ok := drive[field]; !ok {
				t.Errorf("items[%d] missing required DriveSummary field %q", i, field)
			}
		}
	}
}

// validateDriveDetail performs structural validation on the drive detail fixture.
func validateDriveDetail(t *testing.T, m map[string]any) {
	t.Helper()

	driveDetailRequired := []string{
		"id", "vehicleId", "startTime", "endTime", "date",
		"distanceMiles", "durationSeconds", "avgSpeedMph", "maxSpeedMph",
		"energyUsedKwh", "startChargeLevel", "endChargeLevel",
		"fsdMiles", "fsdPercentage", "interventions", "createdAt",
	}

	for _, field := range driveDetailRequired {
		if _, ok := m[field]; !ok {
			t.Errorf("drive_detail missing required DriveDetail field %q", field)
		}
	}
}

// validateDriveRoute performs structural validation on the drive route fixture.
func validateDriveRoute(t *testing.T, m map[string]any) {
	t.Helper()

	if _, ok := m["driveId"]; !ok {
		t.Error("drive_route missing 'driveId'")
	}

	points, ok := m["routePoints"].([]any)
	if !ok {
		t.Error("drive_route missing 'routePoints' array")
		return
	}
	if len(points) == 0 {
		t.Error("drive_route 'routePoints' is empty")
		return
	}

	routePointRequired := []string{"lat", "lng", "speed", "heading", "timestamp"}

	for i, pt := range points {
		point, ok := pt.(map[string]any)
		if !ok {
			t.Errorf("routePoints[%d] is not an object", i)
			continue
		}
		for _, field := range routePointRequired {
			if _, ok := point[field]; !ok {
				t.Errorf("routePoints[%d] missing required RoutePoint field %q", i, field)
			}
		}
	}
}

// validateRESTError validates a REST error envelope has the expected shape.
func validateRESTError(t *testing.T, m map[string]any) {
	t.Helper()

	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Error("REST error fixture missing 'error' object")
		return
	}

	if _, ok := errObj["code"].(string); !ok {
		t.Error("REST error.code missing or not a string")
	}
	if _, ok := errObj["message"].(string); !ok {
		t.Error("REST error.message missing or not a string")
	}
}

// ---------------------------------------------------------------------------
// File globbing helpers
// ---------------------------------------------------------------------------

// mustGlobJSON returns all .json files in dir (non-recursive).
func mustGlobJSON(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no .json files found in %s", dir)
	}
	return files
}

// mustGlobJSONRecursive returns all .json files under dir recursively.
func mustGlobJSONRecursive(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".json") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return files
}
