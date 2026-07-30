//go:build contract

package contract_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadRawSchema reads one schema file as plain JSON, bypassing the compiler.
//
// The compiler resolves and validates STRUCTURE; the custom x-* annotations are
// unknown keywords it ignores entirely. Reading the raw document is the only
// way to assert they are present, which is what makes the classification
// metadata testable rather than merely conventional.
func loadRawSchema(t *testing.T, root, name string) map[string]any {
	t.Helper()
	path := filepath.Join(root, "docs/contracts/schemas", name)
	b, err := os.ReadFile(path) // #nosec G304 -- fixed test path under the repo root
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

// Contract conformance for the MYR-321 saved-places schema surface
// (contracts v0.21.0, docs/contracts/schemas/saved-places.schema.json).
//
// The file is picked up by the schemas/*.json glob every other test in this
// package relies on, which means a broken $ref inside it would silently poison
// UNRELATED validations rather than failing anything of its own. These tests
// compile it explicitly so the new surface has a failure of its own to point at.
//
// What these pin that the Go handler tests cannot: the handler tests prove the
// SERVER behaves, against types the server itself declares. These prove the
// server's shapes match what the SDKs were generated from — the schema in this
// directory is vendored byte-identically into myrobotaxi/contracts, so a drift
// here is a drift in every consumer's generated TypeScript and Swift.

const savedPlacesSchemaID = "https://myrobotaxi.com/schemas/saved-places.schema.json"

// homePlace is the canonical valid row, reused across the tests below.
func homePlaceDoc() map[string]any {
	return map[string]any{
		"kind":      "home",
		"label":     "1 Ferry Building · Embarcadero",
		"latitude":  37.7955,
		"longitude": -122.3937,
	}
}

// withField returns a copy of doc with one key set (or deleted when v is nil),
// so each case mutates one thing without disturbing the others.
func withField(doc map[string]any, key string, v any) map[string]any {
	out := map[string]any{}
	for k, val := range doc {
		out[k] = val
	}
	if v == nil {
		delete(out, key)
	} else {
		out[key] = v
	}
	return out
}

// TestSavedPlacesSchema_Compiles proves the schema and each of its $defs
// resolve. All refs here are LOCAL (#/$defs/...) — unlike vehicle-sharing there
// is no cross-file link, which is deliberate: a saved place is self-contained
// and shares no shape with the ride surface.
func TestSavedPlacesSchema_Compiles(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)

	for _, def := range []string{
		"SavedPlaceKind",
		"SavedPlace",
		"PutSavedPlaceRequest",
	} {
		t.Run(def, func(t *testing.T) {
			if _, err := c.Compile(savedPlacesSchemaID + "#/$defs/" + def); err != nil {
				t.Fatalf("compile $defs/%s: %v", def, err)
			}
		})
	}

	t.Run("SavedPlacesResponse envelope", func(t *testing.T) {
		if _, err := c.Compile(savedPlacesSchemaID); err != nil {
			t.Fatalf("compile root envelope: %v", err)
		}
	})
}

// TestSavedPlaceSchema_RequiresEveryField pins the "no half-set slot" rule the
// migration enforces with NOT NULL columns. A saved place with no coordinate is
// not a saved place: absence is expressed by the row being MISSING from the
// envelope, never by a present row with missing keys.
func TestSavedPlaceSchema_RequiresEveryField(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, savedPlacesSchemaID+"#/$defs/SavedPlace")

	if err := schema.Validate(homePlaceDoc()); err != nil {
		t.Fatalf("the canonical row was rejected: %v", err)
	}

	for _, field := range []string{"kind", "label", "latitude", "longitude"} {
		t.Run("without "+field, func(t *testing.T) {
			if err := schema.Validate(withField(homePlaceDoc(), field, nil)); err == nil {
				t.Fatalf("a row without %s was accepted; every field is required", field)
			}
		})
	}

	t.Run("an unknown field is rejected", func(t *testing.T) {
		// additionalProperties:false. A server that started emitting an extra
		// key would break strict decoders rather than being ignored.
		doc := withField(homePlaceDoc(), "address", "1 Ferry Bldg")
		if err := schema.Validate(doc); err == nil {
			t.Fatal("an unknown field was accepted")
		}
	})
}

// TestSavedPlaceKindSchema_IsTheClosedPair pins the enum against the migration
// 0023 CHECK constraint and the handler's path validation. All three must agree
// or a value legal in one layer is a 500 in another.
func TestSavedPlaceKindSchema_IsTheClosedPair(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, savedPlacesSchemaID+"#/$defs/SavedPlaceKind")

	for _, good := range []string{"home", "work"} {
		if err := schema.Validate(good); err != nil {
			t.Errorf("kind %q rejected: %v", good, err)
		}
	}

	// Case-sensitive and closed. 'Home' is not a synonym — accepting it would
	// let two spellings of one slot reach an upsert whose conflict target is
	// the exact bytes.
	for _, bad := range []any{"Home", "WORK", "gym", "", "home ", 1, nil} {
		if err := schema.Validate(bad); err == nil {
			t.Errorf("kind %v was accepted; the enum is exactly [home, work]", bad)
		}
	}
}

// TestSavedPlaceSchema_CoordinateRangesMatchTheHandler pins the range
// validation to the same bounds internal/telemetry enforces. A schema that
// disagreed would either reject a place the server stored, or promise a client
// a value the server rejects.
func TestSavedPlaceSchema_CoordinateRangesMatchTheHandler(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, savedPlacesSchemaID+"#/$defs/SavedPlace")

	legal := []struct {
		name     string
		lat, lng float64
	}{
		{"north pole", 90, 0},
		{"south pole", -90, 0},
		{"antimeridian east", 0, 180},
		{"antimeridian west", 0, -180},
		// Zero is a REAL coordinate, not an absent one — the schema must not
		// treat it as missing, and the Go decode uses pointers for the same
		// reason.
		{"null island", 0, 0},
	}
	for _, tc := range legal {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			doc := withField(withField(homePlaceDoc(), "latitude", tc.lat), "longitude", tc.lng)
			if err := schema.Validate(doc); err != nil {
				t.Fatalf("boundary coordinate rejected: %v", err)
			}
		})
	}

	illegal := []struct {
		name     string
		lat, lng float64
	}{
		{"latitude above range", 90.1, 0},
		{"latitude below range", -90.1, 0},
		{"longitude above range", 0, 180.1},
		{"longitude below range", 0, -180.1},
	}
	for _, tc := range illegal {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			doc := withField(withField(homePlaceDoc(), "latitude", tc.lat), "longitude", tc.lng)
			if err := schema.Validate(doc); err == nil {
				t.Fatal("an out-of-range coordinate was accepted")
			}
		})
	}
}

// TestSavedPlaceSchema_LabelBounds pins the 1..200 rune window against the
// migration's char_length CHECK and the handler's utf8.RuneCountInString cap.
func TestSavedPlaceSchema_LabelBounds(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, savedPlacesSchemaID+"#/$defs/SavedPlace")

	if err := schema.Validate(withField(homePlaceDoc(), "label", "")); err == nil {
		t.Error("an empty label was accepted; minLength is 1")
	}
	if err := schema.Validate(withField(homePlaceDoc(), "label", strings.Repeat("a", 201))); err == nil {
		t.Error("a 201-character label was accepted; maxLength is 200")
	}
	// Exactly 200 is legal — the bound must not be off by one.
	if err := schema.Validate(withField(homePlaceDoc(), "label", strings.Repeat("a", 200))); err != nil {
		t.Errorf("a 200-character label was rejected: %v", err)
	}
	// maxLength counts CHARACTERS in JSON Schema, so 200 multi-byte runes must
	// pass — matching the handler's rune-count cap rather than a byte cap.
	if err := schema.Validate(withField(homePlaceDoc(), "label", strings.Repeat("é", 200))); err != nil {
		t.Errorf("200 multi-byte characters were rejected: %v", err)
	}
}

// TestSavedPlacesResponseSchema_EnvelopeIsBoundedAndSparse pins the envelope
// the GET handler emits: keyed `places`, capped at two, empty when nothing is
// saved, and sparse when only one kind is.
func TestSavedPlacesResponseSchema_EnvelopeIsBoundedAndSparse(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, savedPlacesSchemaID)

	home := homePlaceDoc()
	work := map[string]any{
		"kind":      "work",
		"label":     "3500 Deer Creek Rd · Palo Alto",
		"latitude":  37.3947,
		"longitude": -122.1503,
	}

	t.Run("both kinds", func(t *testing.T) {
		if err := schema.Validate(map[string]any{"places": []any{home, work}}); err != nil {
			t.Fatalf("a full envelope was rejected: %v", err)
		}
	})

	t.Run("empty for a fresh account", func(t *testing.T) {
		// The state every account starts in, and what the handler emits for a
		// user with no rows. It must validate, not merely be tolerated.
		if err := schema.Validate(map[string]any{"places": []any{}}); err != nil {
			t.Fatalf("an empty envelope was rejected: %v", err)
		}
	})

	t.Run("sparse — one kind set", func(t *testing.T) {
		if err := schema.Validate(map[string]any{"places": []any{work}}); err != nil {
			t.Fatalf("a sparse envelope was rejected: %v", err)
		}
	})

	t.Run("null places is rejected", func(t *testing.T) {
		// The handler builds a non-nil zero-length slice precisely so this
		// shape can never be emitted; the schema is what makes that a contract
		// rather than an implementation detail.
		if err := schema.Validate(map[string]any{"places": nil}); err == nil {
			t.Fatal("places: null was accepted")
		}
	})

	t.Run("a missing places key is rejected", func(t *testing.T) {
		if err := schema.Validate(map[string]any{}); err == nil {
			t.Fatal("an envelope without `places` was accepted")
		}
	})

	t.Run("more than two rows is rejected", func(t *testing.T) {
		// maxItems 2. The set is bounded BY THE ENUM, so a third row means the
		// server produced a kind that should not exist.
		third := withField(home, "kind", "home")
		if err := schema.Validate(map[string]any{"places": []any{home, work, third}}); err == nil {
			t.Fatal("a three-row envelope was accepted; maxItems is 2")
		}
	})

	t.Run("the paginated envelope keys are not part of this shape", func(t *testing.T) {
		// additionalProperties:false on the root. This surface must never grow
		// a cursor: an SDK pagination helper that saw one would page a set that
		// cannot have a second page.
		for _, key := range []string{"items", "nextCursor", "hasMore"} {
			doc := map[string]any{"places": []any{home}, key: "x"}
			if err := schema.Validate(doc); err == nil {
				t.Errorf("the envelope accepted %q", key)
			}
		}
	})
}

// TestPutSavedPlaceRequestSchema_KindIsOptionalAndRedundant pins the request
// body the PUT handler accepts. The slot is named by the PATH; the body key
// exists only so a client holding a whole SavedPlace can post it back
// unstripped.
func TestPutSavedPlaceRequestSchema_KindIsOptionalAndRedundant(t *testing.T) {
	root := repoRoot(t)
	c := newCompiler(t, root)
	schema := compileSchema(t, c, savedPlacesSchemaID+"#/$defs/PutSavedPlaceRequest")

	ordinary := map[string]any{
		"label":     "1 Ferry Building · Embarcadero",
		"latitude":  37.7955,
		"longitude": -122.3937,
	}

	t.Run("the ordinary body omits kind", func(t *testing.T) {
		if err := schema.Validate(ordinary); err != nil {
			t.Fatalf("a body without kind was rejected: %v", err)
		}
	})

	t.Run("kind may be echoed back", func(t *testing.T) {
		if err := schema.Validate(withField(ordinary, "kind", "home")); err != nil {
			t.Fatalf("a body with kind was rejected: %v", err)
		}
	})

	t.Run("an invalid kind is still rejected when present", func(t *testing.T) {
		// Optional does not mean unvalidated: the path/body agreement check is
		// the handler's job, but the MEMBERSHIP check is the schema's.
		if err := schema.Validate(withField(ordinary, "kind", "gym")); err == nil {
			t.Fatal("kind 'gym' was accepted")
		}
	})

	for _, field := range []string{"label", "latitude", "longitude"} {
		t.Run("without "+field, func(t *testing.T) {
			// Whole-object upsert: an omitted field means "I did not send one",
			// never "keep the stored value". Contrast the §7.19 push-prefs PUT,
			// where every key is genuinely optional.
			if err := schema.Validate(withField(ordinary, field, nil)); err == nil {
				t.Fatalf("a body without %s was accepted; the write is an upsert, not a patch", field)
			}
		})
	}

	t.Run("a userId key is rejected", func(t *testing.T) {
		// additionalProperties:false. This is a /users/me surface: the owning
		// account is the JWT subject and is never client-supplied.
		if err := schema.Validate(withField(ordinary, "userId", "cusr_someone_else")); err == nil {
			t.Fatal("a body naming another user was accepted")
		}
	})
}

// TestSavedPlacesSchema_CoordinatesCarryTheEncryptionAnnotations pins the
// classification metadata itself, not just the shape.
//
// The x-classification / x-encrypted-at-rest annotations are what the codegen
// pipelines fold into TSDoc and SwiftDoc, and what data-classification.md §1.17
// is cross-checked against. Losing them would not break a single validation —
// which is exactly why they need a test that fails when they go.
func TestSavedPlacesSchema_CoordinatesCarryTheEncryptionAnnotations(t *testing.T) {
	root := repoRoot(t)
	raw := loadRawSchema(t, root, "saved-places.schema.json")

	defs, ok := raw["$defs"].(map[string]any)
	if !ok {
		t.Fatal("schema has no $defs")
	}
	place, ok := defs["SavedPlace"].(map[string]any)
	if !ok {
		t.Fatal("$defs.SavedPlace missing")
	}
	props, ok := place["properties"].(map[string]any)
	if !ok {
		t.Fatal("$defs.SavedPlace.properties missing")
	}

	for _, field := range []string{"latitude", "longitude"} {
		t.Run(field, func(t *testing.T) {
			prop, ok := props[field].(map[string]any)
			if !ok {
				t.Fatalf("property %s missing", field)
			}
			if got := prop["x-classification"]; got != "P1" {
				t.Errorf("x-classification = %v, want P1", got)
			}
			if got := prop["x-encrypted-at-rest"]; got != true {
				t.Errorf("x-encrypted-at-rest = %v, want true — these columns are encrypt-only", got)
			}
			if got := prop["x-unit"]; got != "degrees" {
				t.Errorf("x-unit = %v, want degrees", got)
			}
		})
	}

	t.Run("label is P1 but NOT encrypted", func(t *testing.T) {
		// The tier split go_ride_requests makes between pickup_lat_enc and
		// pickup_label. Asserting the ABSENCE matters as much as the presence:
		// a label that acquired x-encrypted-at-rest would put the schema out of
		// step with a column that is stored in the clear.
		prop, ok := props["label"].(map[string]any)
		if !ok {
			t.Fatal("property label missing")
		}
		if got := prop["x-classification"]; got != "P1" {
			t.Errorf("x-classification = %v, want P1", got)
		}
		if _, encrypted := prop["x-encrypted-at-rest"]; encrypted {
			t.Error("label is marked encrypted-at-rest, but the column stores plaintext")
		}
	})
}
