package plaintextpurge

// Unit tests for the purge's safety argument.
//
// Everything here is about ONE question: when is it safe to destroy a
// plaintext value? Getting that wrong in either direction is bad —
// refusing to purge leaves credentials readable, purging too eagerly
// deletes the only copy of a user's data — so the rules get tested
// directly rather than only through the integration test.

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
			// The whole reason KindFloat exists. Postgres renders a double
			// precision column its own way; Go's FormatFloat(-1) renders it
			// another. A text comparison would call a healthy row stale.
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
			name: "float differing in last digit", kind: KindFloat,
			plaintext: "33.0975241", decrypted: "33.0975242", want: false,
		},
		{
			name: "float unparseable plaintext does not match", kind: KindFloat,
			plaintext: "not-a-number", decrypted: "33.1", want: false,
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
			name: "json with a dropped element differs", kind: KindJSON,
			plaintext: `[[1,2],[3,4]]`, decrypted: `[[1,2]]`, want: false,
		},
		{
			name: "json empty arrays match", kind: KindJSON,
			plaintext: `[]`, decrypted: `[]`, want: true,
		},
		{
			name: "json invalid decryption does not match", kind: KindJSON,
			plaintext: `[[1,2]]`, decrypted: "not json", want: false,
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

// TestVerifyTarget covers the verdict a row receives, which is what
// decides whether its plaintext is destroyed.
func TestVerifyTarget(t *testing.T) {
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
	empty := ""

	tokenTarget := targetNamed(t, "Account.access_token")
	gpsTarget := targetNamed(t, "Vehicle.latitude+longitude")

	tests := []struct {
		name        string
		target      Target
		hasData     []bool
		plaintexts  []*string
		ciphertexts []*string
		want        verdict
		wantStale   bool
	}{
		{
			name:    "token sealed and identical is scrubbed as redundant",
			target:  tokenTarget,
			hasData: []bool{true}, plaintexts: []*string{ptr("qts-abc")},
			ciphertexts: []*string{sealString(t, "qts-abc")},
			want:        verdictScrub, wantStale: false,
		},
		{
			// THE post-deploy case. The server refreshed the token, so the
			// ciphertext advanced and the frozen plaintext no longer
			// matches. This MUST scrub — blocking it would leave a live
			// Tesla credential readable forever.
			name:    "token whose ciphertext advanced is scrubbed as stale",
			target:  tokenTarget,
			hasData: []bool{true}, plaintexts: []*string{ptr("qts-OLD")},
			ciphertexts: []*string{sealString(t, "qts-REFRESHED")},
			want:        verdictScrub, wantStale: true,
		},
		{
			// The dangerous case. No ciphertext means the plaintext is the
			// only copy in existence.
			name:    "token with no ciphertext is left alone",
			target:  tokenTarget,
			hasData: []bool{true}, plaintexts: []*string{ptr("qts-abc")},
			ciphertexts: []*string{nil},
			want:        verdictUnsealed,
		},
		{
			name:    "token with empty-string ciphertext is left alone",
			target:  tokenTarget,
			hasData: []bool{true}, plaintexts: []*string{ptr("qts-abc")},
			ciphertexts: []*string{&empty},
			want:        verdictUnsealed,
		},
		{
			name:    "garbage ciphertext is left alone",
			target:  tokenTarget,
			hasData: []bool{true}, plaintexts: []*string{ptr("qts-abc")},
			ciphertexts: []*string{ptr("not-base64-at-all!!")},
			want:        verdictUndecryptable,
		},
		{
			name:    "well-formed base64 that is not our ciphertext is left alone",
			target:  tokenTarget,
			hasData: []bool{true}, plaintexts: []*string{ptr("qts-abc")},
			ciphertexts: []*string{ptr(base64.StdEncoding.EncodeToString([]byte("0123456789abcdefghijklmnop")))},
			want:        verdictUndecryptable,
		},
		{
			name:    "GPS pair both sealed is scrubbed",
			target:  gpsTarget,
			hasData: []bool{true, true}, plaintexts: []*string{ptr("33.1"), ptr("-96.8")},
			ciphertexts: []*string{sealString(t, "33.1"), sealString(t, "-96.8")},
			want:        verdictScrub,
		},
		{
			name:    "GPS pair that both advanced is scrubbed as stale",
			target:  gpsTarget,
			hasData: []bool{true, true}, plaintexts: []*string{ptr("33.1"), ptr("-96.8")},
			ciphertexts: []*string{sealString(t, "34.5"), sealString(t, "-97.2")},
			want:        verdictScrub, wantStale: true,
		},
		{
			// The pair invariant. Scrubbing the verified half alone would
			// leave a readable longitude AND destroy half the pair.
			name:    "GPS pair blocks entirely when one half is unsealed",
			target:  gpsTarget,
			hasData: []bool{true, true}, plaintexts: []*string{ptr("33.1"), ptr("-96.8")},
			ciphertexts: []*string{sealString(t, "33.1"), nil},
			want:        verdictUnsealed,
		},
		{
			name:    "GPS pair blocks entirely when one half is undecryptable",
			target:  gpsTarget,
			hasData: []bool{true, true}, plaintexts: []*string{ptr("33.1"), ptr("-96.8")},
			ciphertexts: []*string{sealString(t, "33.1"), ptr("garbage!!")},
			want:        verdictUndecryptable,
		},
		{
			// One half already scrubbed to 0 — hasData false. That half
			// needs no ciphertext and must not block the other.
			name:    "GPS pair with one half already scrubbed still purges the other",
			target:  gpsTarget,
			hasData: []bool{false, true}, plaintexts: []*string{ptr("0"), ptr("-96.8")},
			ciphertexts: []*string{nil, sealString(t, "-96.8")},
			want:        verdictScrub,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, stale := p.verifyTarget("row_1",
				zipScans(tt.target, tt.hasData, tt.plaintexts, tt.ciphertexts))
			if got != tt.want {
				t.Errorf("verifyTarget() verdict = %v, want %v", got, tt.want)
			}
			if got == verdictScrub && stale != tt.wantStale {
				t.Errorf("verifyTarget() stale = %v, want %v", stale, tt.wantStale)
			}
		})
	}
}

// TestVerifyRejectsWrongKey pins the behaviour that matters most during a
// key rotation: a purge run with the wrong key must destroy nothing.
//
// This is the sharp edge of the "decryptable ⇒ scrub" rule. Because a
// mismatch now scrubs, the ONLY thing standing between a wrong key and
// mass data loss is that a wrong key fails to decrypt at all. AES-GCM's
// auth tag guarantees that, and this test pins it.
func TestVerifyRejectsWrongKey(t *testing.T) {
	writer := newTestEncryptor(t)
	reader := newTestEncryptor(t) // a different random key

	ct, err := writer.EncryptString("qts-abc")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	p := &Purger{encryptor: reader}
	got, _ := p.verifyTarget("row_1", zipScans(targetNamed(t, "Account.access_token"),
		[]bool{true}, []*string{ptr("qts-abc")}, []*string{&ct}))
	if got != verdictUndecryptable {
		t.Errorf("verifyTarget() with the wrong key = %v, want verdictUndecryptable", got)
	}
}

// TestVerifyJSONTargetStaleIsScrubbed covers the route-blob shape of the
// post-deploy skew: a drive whose trail kept growing in ciphertext while
// the plaintext column stayed at its pre-deploy snapshot.
func TestVerifyJSONTargetStaleIsScrubbed(t *testing.T) {
	enc := newTestEncryptor(t)
	p := &Purger{encryptor: enc}

	grown, err := routeblob.EncryptJSONBytes([]byte(`[{"lat":1},{"lat":2},{"lat":3}]`), enc)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	got, stale := p.verifyTarget("drv_1", zipScans(targetNamed(t, "Drive.routePoints"),
		[]bool{true}, []*string{ptr(`[{"lat":1}]`)}, []*string{&grown}))
	if got != verdictScrub {
		t.Fatalf("verifyTarget() = %v, want verdictScrub — a trail that grew in ciphertext "+
			"must not pin its stale plaintext in place", got)
	}
	if !stale {
		t.Error("verifyTarget() reported stale=false; the plaintext was a strict prefix of the trail")
	}
}

// TestTargetsCoverEveryRetiredColumn guards the table itself. A column
// that MYR-433 stopped writing but that nobody added here would keep its
// plaintext forever, silently.
func TestTargetsCoverEveryRetiredColumn(t *testing.T) {
	want := []string{
		"access_token", "refresh_token", "id_token",
		"latitude", "longitude",
		"destinationLatitude", "destinationLongitude",
		"originLatitude", "originLongitude",
		"navRouteCoordinates", "routePoints",
		// MYR-447 — the geocoded rendering of the coordinates above. A
		// street address left readable makes the sealed coordinate beside
		// it decorative, so these are in scope for the same purge.
		"locationName", "locationAddress",
		"destinationName", "destinationAddress",
		"startLocation", "startAddress",
		"endLocation", "endAddress",
	}

	got := map[string]bool{}
	for _, tgt := range Targets {
		for _, m := range tgt.Members {
			got[m.Plaintext] = true
		}
	}
	for _, col := range want {
		if !got[col] {
			t.Errorf("Targets is missing %s — its plaintext would never be purged", col)
		}
	}
	if len(got) != len(want) {
		t.Errorf("Targets covers %d columns, want %d", len(got), len(want))
	}

	// The credential targets must come first: if a run is interrupted, the
	// Tesla tokens should already be gone.
	for i := 0; i < 3; i++ {
		if Targets[i].Table != "Account" {
			t.Errorf("Targets[%d] is %s; the three Account token targets must be purged first",
				i, Targets[i].Label())
		}
	}
}

// TestGPSPairsArePairedTargets pins that each lat/lng pair is ONE target.
// Split into two targets, a half-verifying pair would leave one readable
// coordinate behind.
func TestGPSPairsArePairedTargets(t *testing.T) {
	pairs := map[string][2]string{
		"Vehicle.latitude+longitude":                       {"latitude", "longitude"},
		"Vehicle.destinationLatitude+destinationLongitude": {"destinationLatitude", "destinationLongitude"},
		"Vehicle.originLatitude+originLongitude":           {"originLatitude", "originLongitude"},
	}
	for label, want := range pairs {
		tgt := targetNamed(t, label)
		if len(tgt.Members) != 2 {
			t.Errorf("%s has %d members, want 2 — GPS halves must be decided together",
				label, len(tgt.Members))
			continue
		}
		if tgt.Members[0].Plaintext != want[0] || tgt.Members[1].Plaintext != want[1] {
			t.Errorf("%s members = %s/%s, want %s/%s",
				label, tgt.Members[0].Plaintext, tgt.Members[1].Plaintext, want[0], want[1])
		}
	}
}

// TestNotNullColumnsDoNotScrubToNull pins the constraint that would
// otherwise abort the purge at runtime: three columns are NOT NULL on the
// Prisma schema and cannot be set to NULL.
func TestNotNullColumnsDoNotScrubToNull(t *testing.T) {
	notNull := map[string]string{
		"latitude":    "0",
		"longitude":   "0",
		"routePoints": `'[]'::jsonb`,
	}
	for _, tgt := range Targets {
		for _, m := range tgt.Members {
			want, isNotNull := notNull[m.Plaintext]
			if !isNotNull {
				continue
			}
			if m.ScrubSQL != want {
				t.Errorf("%s scrubs to %q, want %q — it is NOT NULL on the Prisma schema",
					m.Plaintext, m.ScrubSQL, want)
			}
		}
	}
}

// TestHasDataPredicatesNameTheirColumn is a cheap consistency check: a
// predicate referencing the wrong column would make the purge report a
// clean sweep it never performed.
func TestHasDataPredicatesNameTheirColumn(t *testing.T) {
	for _, tgt := range Targets {
		for _, m := range tgt.Members {
			if !strings.Contains(m.HasData, `"`+m.Plaintext+`"`) {
				t.Errorf("%s: HasData %q does not reference the column", m.Plaintext, m.HasData)
			}
		}
	}
}

// TestBuildScrubSQLGuardsEveryCiphertext pins that the scrub UPDATE
// re-checks every member's ciphertext, so a row whose shadow vanished
// between the SELECT and the UPDATE is not scrubbed anyway.
func TestBuildScrubSQLGuardsEveryCiphertext(t *testing.T) {
	sql := buildScrubSQL(targetNamed(t, "Vehicle.latitude+longitude"))
	for _, want := range []string{
		`"latitude" = 0`, `"longitude" = 0`,
		`"latitudeEnc" IS NOT NULL`, `"longitudeEnc" IS NOT NULL`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("scrub SQL %q is missing %q", sql, want)
		}
	}
}

// zipScans builds the []memberScan the verifier takes from the
// per-member columns a test case declares. Keeping the test tables as
// parallel slices stays readable; the production path never has them.
func zipScans(tgt Target, hasData []bool, plaintexts, ciphertexts []*string) []memberScan {
	scans := make([]memberScan, 0, len(tgt.Members))
	for i, m := range tgt.Members {
		s := memberScan{member: m}
		if i < len(hasData) {
			s.hasData = hasData[i]
		}
		if i < len(plaintexts) {
			s.plaintext = plaintexts[i]
		}
		if i < len(ciphertexts) {
			s.ciphertext = ciphertexts[i]
		}
		scans = append(scans, s)
	}
	return scans
}

// targetNamed looks up a Target by label, failing the test if absent.
func targetNamed(t *testing.T, label string) Target {
	t.Helper()
	for _, tgt := range Targets {
		if tgt.Label() == label {
			return tgt
		}
	}
	t.Fatalf("no target labelled %q", label)
	return Target{}
}

// newTestEncryptor builds an AES-256-GCM Encryptor over a random key,
// going through the production loader so the test exercises the real key
// path rather than a bespoke constructor.
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
