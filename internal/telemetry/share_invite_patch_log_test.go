package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

// MYR-451: requestedFlag renders an optional patch field for the incident log.
//
// The invariant under test is not "it prints the right word" — it is that ONE
// KEY CARRIES ONE JSON TYPE across all three states. Production logs JSON, and
// a key that is a boolean on one line and a string on the next is rejected by
// any strict-mapping sink (Elasticsearch dynamic mapping, BigQuery, Loki
// structured metadata), which DROPS the document — silently discarding exactly
// the line written to settle an incident.
//
// That failure is invisible in a unit test that only inspects Go values, so
// this one round-trips through the real JSON handler and asserts on the decoded
// type. It exists to break a future "simplify this to slog.Any" refactor, which
// would reintroduce both this bug and the pointer-printing one the docstring
// describes.
func TestRequestedFlag_OneKeyOneJSONType(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name  string
		value *bool
		want  string
	}{
		{name: "absent renders unchanged, never false", value: nil, want: "unchanged"},
		{name: "present true", value: &yes, want: "true"},
		{name: "present false", value: &no, want: "false"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			logger.LogAttrs(context.Background(), slog.LevelInfo, "patched",
				requestedFlag("requested_allow_rides", tc.value))

			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("log line is not valid JSON: %v", err)
			}

			// The TYPE assertion is the point: a bool here would decode as
			// bool, not string, and fail before the value is ever compared.
			field, ok := got["requested_allow_rides"].(string)
			if !ok {
				t.Fatalf("requested_allow_rides decoded as %T, want string — "+
					"a key whose JSON type varies by state is dropped by strict-mapping sinks",
					got["requested_allow_rides"])
			}
			if field != tc.want {
				t.Errorf("requested_allow_rides = %q, want %q", field, tc.want)
			}
		})
	}
}
