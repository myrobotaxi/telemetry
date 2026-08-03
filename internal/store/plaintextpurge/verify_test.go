package plaintextpurge

// Unit tests for the purge's safety argument.
//
// Everything here is about ONE question: when is it safe to destroy a
// plaintext value? Getting that wrong in either direction is bad —
// refusing to purge leaves credentials readable, purging too eagerly
// deletes the only copy of a user's data — so the comparison rules get
// tested directly rather than only through the integration test.

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store/routeblob"
)

func TestValuesMatch(t *testing.T) {
	tests := []struct {
		name      string
		kind      Kind
		plaintext string
		decrypted string
		want      bool
	}{
		{
			name: "string identical", kind: KindString,
			plaintext: "qts-abc123", decrypted: "qts-abc123", want: true,
		},
		{
			name: "string differing by one character", kind: KindString,
			plaintext: "qts-abc123", decrypted: "qts-abc124", want: false,
		},
		{
			name: "string comparison is exact, not trimmed", kind: KindString,
			plaintext: "qts-abc123", decrypted: "qts-abc123 ", want: false,
		},
		{
			name: "empty string matches empty string", kind: KindString,
			plaintext: "", decrypted: "", want: true,
		},
		{
			// The whole reason KindFloat exists. Postgres renders a double
			// precision column its own way; Go's FormatFloat(-1) renders it
			// another. A text comparison would refuse to purge a perfectly
			// healthy row.
			name: "float equal despite different text rendering", kind: KindFloat,
			plaintext: "33.10", decrypted: "33.1", want: true,
		},
		{
			name: "float negative round-trip", kind: KindFloat,
			plaintext: "-96.8214773", decrypted: "-96.8214773", want: true,
		},
		{
			name: "float exponent form equals decimal form", kind: KindFloat,
			plaintext: "1e2", decrypted: "100", want: true,
		},
		{
			// A hair's difference is still a different place. Do not purge.
			name: "float differing in last digit", kind: KindFloat,
			plaintext: "33.0975241", decrypted: "33.0975242", want: false,
		},
		{
			name: "float unparseable plaintext is not a match", kind: KindFloat,
			plaintext: "not-a-number", decrypted: "33.1", want: false,
		},
		{
			name: "float unparseable decryption is not a match", kind: KindFloat,
			plaintext: "33.1", decrypted: "", want: false,
		},
		{
			// The jsonb normalisation case: Postgres strips whitespace on
			// storage, so the plaintext read-back legitimately differs
			// byte-wise from what was sealed.
			name: "json equal ignoring whitespace", kind: KindJSON,
			plaintext: `[[1,2],[3,4]]`, decrypted: "[ [1, 2], [3, 4] ]", want: true,
		},
		{
			name: "json object key order does not matter", kind: KindJSON,
			plaintext: `{"lat":1,"lng":2}`, decrypted: `{"lng":2,"lat":1}`, want: true,
		},
		{
			name: "json with a dropped element is not a match", kind: KindJSON,
			plaintext: `[[1,2],[3,4]]`, decrypted: `[[1,2]]`, want: false,
		},
		{
			name: "json with a changed coordinate is not a match", kind: KindJSON,
			plaintext: `[[1,2],[3,4]]`, decrypted: `[[1,2],[3,5]]`, want: false,
		},
		{
			name: "json empty arrays match", kind: KindJSON,
			plaintext: `[]`, decrypted: `[]`, want: true,
		},
		{
			name: "json invalid decryption is not a match", kind: KindJSON,
			plaintext: `[[1,2]]`, decrypted: "not json", want: false,
		},
		{
			name: "json invalid plaintext is not a match", kind: KindJSON,
			plaintext: "not json", decrypted: `[[1,2]]`, want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := valuesMatch(tt.kind, tt.plaintext, tt.decrypted); got != tt.want {
				t.Errorf("valuesMatch(%v, %q, %q) = %v, want %v",
					tt.kind, tt.plaintext, tt.decrypted, got, tt.want)
			}
		})
	}
}

// TestVerify covers the verdict a row receives, which is what decides
// whether its plaintext is destroyed.
func TestVerify(t *testing.T) {
	enc := newTestEncryptor(t)
	p := &Purger{encryptor: enc}

	sealString := func(t *testing.T, s string) *string {
		t.Helper()
		ct, err := enc.EncryptString(s)
		if err != nil {
			t.Fatalf("seal %q: %v", s, err)
		}
		return &ct
	}
	sealJSON := func(t *testing.T, s string) *string {
		t.Helper()
		ct, err := routeblob.EncryptJSONBytes([]byte(s), enc)
		if err != nil {
			t.Fatalf("seal json %q: %v", s, err)
		}
		return &ct
	}
	empty := ""

	tokenCol := Columns[0] // Account.access_token, KindString
	gpsCol := Columns[3]   // Vehicle.latitude, KindFloat
	jsonCol := Columns[10] // Drive.routePoints, KindJSON

	tests := []struct {
		name       string
		col        Column
		plaintext  string
		ciphertext *string
		want       verdict
	}{
		{
			name: "token sealed and matching purges",
			col:  tokenCol, plaintext: "qts-abc", ciphertext: sealString(t, "qts-abc"),
			want: verdictOK,
		},
		{
			// The dangerous case. No ciphertext means the plaintext is the
			// only copy in existence.
			name: "token with no ciphertext is left alone",
			col:  tokenCol, plaintext: "qts-abc", ciphertext: nil,
			want: verdictUnsealed,
		},
		{
			name: "token with empty ciphertext is left alone",
			col:  tokenCol, plaintext: "qts-abc", ciphertext: &empty,
			want: verdictUnsealed,
		},
		{
			name: "token whose ciphertext holds a different value is left alone",
			col:  tokenCol, plaintext: "qts-abc", ciphertext: sealString(t, "qts-DIFFERENT"),
			want: verdictMismatch,
		},
		{
			name: "garbage ciphertext is left alone",
			col:  tokenCol, plaintext: "qts-abc", ciphertext: ptr("not-base64-at-all!!"),
			want: verdictUndecryptable,
		},
		{
			name: "well-formed base64 that is not our ciphertext is left alone",
			col:  tokenCol, plaintext: "qts-abc",
			ciphertext: ptr(base64.StdEncoding.EncodeToString([]byte("0123456789abcdefghijklmnop"))),
			want:       verdictUndecryptable,
		},
		{
			name: "coordinate sealed and matching purges",
			col:  gpsCol, plaintext: "33.1", ciphertext: sealString(t, "33.1"),
			want: verdictOK,
		},
		{
			name: "coordinate matching despite text rendering purges",
			col:  gpsCol, plaintext: "33.10", ciphertext: sealString(t, "33.1"),
			want: verdictOK,
		},
		{
			name: "coordinate off by a digit is left alone",
			col:  gpsCol, plaintext: "33.1", ciphertext: sealString(t, "33.2"),
			want: verdictMismatch,
		},
		{
			name: "trail sealed and matching purges",
			col:  jsonCol, plaintext: `[{"lat":1,"lng":2}]`, ciphertext: sealJSON(t, `[{"lat":1,"lng":2}]`),
			want: verdictOK,
		},
		{
			name: "trail matching despite jsonb normalisation purges",
			col:  jsonCol, plaintext: `[{"lat": 1, "lng": 2}]`, ciphertext: sealJSON(t, `[{"lng":2,"lat":1}]`),
			want: verdictOK,
		},
		{
			name: "truncated trail is left alone",
			col:  jsonCol, plaintext: `[{"lat":1},{"lat":2}]`, ciphertext: sealJSON(t, `[{"lat":1}]`),
			want: verdictMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.verify(tt.col, "row_1", tt.plaintext, tt.ciphertext); got != tt.want {
				t.Errorf("verify() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestVerifyRejectsWrongKey pins the behaviour that matters most during a
// key rotation: a purge run with the wrong key must destroy nothing.
//
// Without this the failure mode is silent and total — every row looks
// undecryptable, and a purge that treated "cannot verify" as "safe to
// remove" would wipe every token and coordinate in the database.
func TestVerifyRejectsWrongKey(t *testing.T) {
	writer := newTestEncryptor(t)
	reader := newTestEncryptor(t) // a different random key

	ct, err := writer.EncryptString("qts-abc")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	p := &Purger{encryptor: reader}
	if got := p.verify(Columns[0], "row_1", "qts-abc", &ct); got != verdictUndecryptable {
		t.Errorf("verify() with the wrong key = %v, want verdictUndecryptable", got)
	}
}

// TestColumnsCoverEveryRetiredColumn guards the table itself. A column
// that MYR-433 stopped writing but that nobody added here would keep its
// plaintext forever, silently.
func TestColumnsCoverEveryRetiredColumn(t *testing.T) {
	want := []string{
		"Account.access_token",
		"Account.refresh_token",
		"Account.id_token",
		"Vehicle.latitude",
		"Vehicle.longitude",
		"Vehicle.destinationLatitude",
		"Vehicle.destinationLongitude",
		"Vehicle.originLatitude",
		"Vehicle.originLongitude",
		"Vehicle.navRouteCoordinates",
		"Drive.routePoints",
	}

	got := make(map[string]bool, len(Columns))
	for _, c := range Columns {
		got[c.Label()] = true
	}
	for _, label := range want {
		if !got[label] {
			t.Errorf("Columns is missing %s — its plaintext would never be purged", label)
		}
	}
	if len(Columns) != len(want) {
		t.Errorf("Columns has %d entries, want %d", len(Columns), len(want))
	}

	// The credential columns must come first: if a run is interrupted, the
	// Tesla tokens should already be gone.
	for i := 0; i < 3; i++ {
		if Columns[i].Table != "Account" {
			t.Errorf("Columns[%d] is %s; the three Account token columns must be purged first",
				i, Columns[i].Label())
		}
	}
}

// TestNotNullColumnsDoNotScrubToNull pins the constraint that would
// otherwise abort the purge at runtime: three columns are NOT NULL on the
// Prisma schema and cannot be set to NULL.
func TestNotNullColumnsDoNotScrubToNull(t *testing.T) {
	notNull := map[string]string{
		"Vehicle.latitude":  "0",
		"Vehicle.longitude": "0",
		"Drive.routePoints": `'[]'::jsonb`,
	}
	for _, c := range Columns {
		want, isNotNull := notNull[c.Label()]
		if !isNotNull {
			continue
		}
		if c.ScrubSQL != want {
			t.Errorf("%s scrubs to %q, want %q — it is NOT NULL on the Prisma schema",
				c.Label(), c.ScrubSQL, want)
		}
	}
}

// TestRemainingPredicatesNameTheirColumn is a cheap consistency check: a
// predicate that referenced the wrong column would make the purge report
// a clean sweep it never performed.
func TestRemainingPredicatesNameTheirColumn(t *testing.T) {
	for _, c := range Columns {
		if !strings.Contains(c.RemainingPredicate, `"`+c.Plaintext+`"`) {
			t.Errorf("%s: RemainingPredicate %q does not reference the column",
				c.Label(), c.RemainingPredicate)
		}
	}
}

// newTestEncryptor builds an AES-256-GCM Encryptor over a random key,
// going through the production loader so the test exercises the real
// key path rather than a bespoke constructor.
func newTestEncryptor(t *testing.T) cryptox.Encryptor {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(raw))
	ks, err := cryptox.LoadKeySetFromEnv()
	if err != nil {
		t.Fatalf("LoadKeySetFromEnv: %v", err)
	}
	enc, err := cryptox.NewEncryptor(ks)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

func ptr(s string) *string { return &s }
