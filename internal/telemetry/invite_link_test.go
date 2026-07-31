package telemetry

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Signed invite links (MYR-368). The point of the design is that the web join
// shell can reject a forged, tampered, or expired link with a COMPILED-IN
// public key and no server round trip, so these tests verify against a raw
// ed25519.PublicKey and rebuild the canonical payload BY HAND rather than
// through the helper the signer uses — a bug shared by both sides would
// otherwise pass, and the web verifier is written from this canonical form.
const (
	testCode = "RBO246"
	testFrom = "Alex"
	testTo   = "Mira"
)

var testExpiry = time.Date(2026, 8, 5, 15, 4, 5, 0, time.UTC)

// newTestSigner returns a signer over a freshly generated key plus the public
// half. Tests NEVER read INVITE_LINK_SIGNING_KEY: a generated key makes every
// case hermetic and keeps a real seed out of the repo.
func newTestSigner(t *testing.T) (*InviteLinkSigner, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := NewInviteLinkSigner(priv.Seed())
	if err != nil {
		t.Fatalf("NewInviteLinkSigner: %v", err)
	}
	return signer, pub
}

// TestInviteLinkSigner_URLShape pins the wire shape of the whole link, because
// the web shell parses it by position: path segment = code, `k` = three
// dot-separated parts (key id, expiry, signature), then the two display names.
func TestInviteLinkSigner_URLShape(t *testing.T) {
	signer, _ := newTestSigner(t)

	got := signer.ShareURL(testCode, testExpiry, "Alex Rivera", "Mira Chen")

	if !strings.HasPrefix(got, "https://myrobotaxi.app/join/RBO246?k=") {
		t.Fatalf("unexpected URL prefix: %q", got)
	}
	// Parameter ORDER is contractual — k, from, to — so the link reads the
	// same way every time a human looks at one.
	if !strings.HasSuffix(got, "&from=Alex&to=Mira") {
		t.Errorf("expected the name params in k,from,to order: %q", got)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if u.Path != "/join/RBO246" {
		t.Errorf("path = %q, want /join/RBO246", u.Path)
	}
	if got, want := u.Query().Get("from"), testFrom; got != want {
		t.Errorf("from = %q, want %q", got, want)
	}
	if got, want := u.Query().Get("to"), testTo; got != want {
		t.Errorf("to = %q, want %q", got, want)
	}

	k := u.Query().Get("k")
	parts := strings.Split(k, ".")
	if len(parts) != 3 {
		t.Fatalf("k = %q, want three dot-separated parts", k)
	}
	if parts[0] != "1" {
		t.Errorf("key id = %q, want \"1\"", parts[0])
	}
	if parts[1] != strconv.FormatInt(testExpiry.Unix(), 10) {
		t.Errorf("exp = %q, want %d", parts[1], testExpiry.Unix())
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature is not unpadded base64url: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Errorf("signature length = %d, want %d", len(sig), ed25519.SignatureSize)
	}
	// Nothing in the emitted link needs percent-encoding: base64url and the
	// sanitized name alphabet are both URL-safe. A '%' here would mean the
	// shell reading the raw query sees different bytes than were signed.
	if strings.Contains(got, "%") {
		t.Errorf("URL contains percent-encoding: %q", got)
	}
}

// TestInviteLinkSigner_SignatureVerifies is the security property: the
// signature over the canonical payload verifies with the PUBLIC key alone.
func TestInviteLinkSigner_SignatureVerifies(t *testing.T) {
	signer, pub := newTestSigner(t)
	shareURL := signer.ShareURL(testCode, testExpiry, "Alex Rivera", "Mira Chen")
	exp := strconv.FormatInt(testExpiry.Unix(), 10)
	sig := sigBytes(t, shareURL)

	payload := "join:" + testCode + ":" + exp + ":" + testFrom + ":" + testTo
	if !ed25519.Verify(pub, []byte(payload), sig) {
		t.Fatalf("signature does not verify over canonical payload %q", payload)
	}

	// Every field of the payload is load-bearing: flipping any one of them
	// must break verification. These are the tamper cases the shell relies
	// on — in particular the two NAME swaps, which are what an attacker
	// holding a genuine link would try.
	tampered := []struct {
		name    string
		payload string
	}{
		{"swapped code", "join:XXX999:" + exp + ":" + testFrom + ":" + testTo},
		{"swapped expiry", "join:" + testCode + ":" + strconv.FormatInt(testExpiry.Unix()+1, 10) + ":" + testFrom + ":" + testTo},
		{"swapped from", "join:" + testCode + ":" + exp + ":Mallory:" + testTo},
		{"swapped to", "join:" + testCode + ":" + exp + ":" + testFrom + ":Mallory"},
		{"names transposed", "join:" + testCode + ":" + exp + ":" + testTo + ":" + testFrom},
		{"names dropped", "join:" + testCode + ":" + exp + "::"},
		{"missing prefix", testCode + ":" + exp + ":" + testFrom + ":" + testTo},
	}
	for _, tc := range tampered {
		t.Run(tc.name+" does not verify", func(t *testing.T) {
			if ed25519.Verify(pub, []byte(tc.payload), sig) {
				t.Fatalf("signature verified over tampered payload %q", tc.payload)
			}
		})
	}

	t.Run("another key does not verify", func(t *testing.T) {
		otherPub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		if ed25519.Verify(otherPub, []byte(payload), sig) {
			t.Fatal("signature verified under an unrelated public key")
		}
	})
}

// TestInviteLinkSigner_OmittedNamesCanonicalForm pins what an absent name
// means on both sides: the parameter is GONE from the URL (never an empty
// `from=`), and the payload field is the EMPTY STRING, so the payload always
// has five fields and four colons.
func TestInviteLinkSigner_OmittedNamesCanonicalForm(t *testing.T) {
	signer, pub := newTestSigner(t)
	exp := strconv.FormatInt(testExpiry.Unix(), 10)

	cases := []struct {
		name        string
		owner       string
		label       string
		wantQuery   string
		wantPayload string
	}{
		{
			name: "both omitted", owner: "🙂", label: "123",
			wantQuery:   "",
			wantPayload: "join:" + testCode + ":" + exp + "::",
		},
		{
			name: "from only", owner: "Alex", label: "!!!",
			wantQuery:   "&from=Alex",
			wantPayload: "join:" + testCode + ":" + exp + ":Alex:",
		},
		{
			name: "to only", owner: "", label: "Mira",
			wantQuery:   "&to=Mira",
			wantPayload: "join:" + testCode + ":" + exp + "::Mira",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := signer.ShareURL(testCode, testExpiry, tc.owner, tc.label)
			if strings.Contains(got, "from=&") || strings.HasSuffix(got, "from=") ||
				strings.Contains(got, "to=&") || strings.HasSuffix(got, "to=") {
				t.Errorf("emitted an empty name parameter: %q", got)
			}
			if _, query, _ := strings.Cut(got, "&"); tc.wantQuery == "" {
				if strings.Contains(got, "&") {
					t.Errorf("expected no name params, got %q", got)
				}
			} else if "&"+query != tc.wantQuery {
				t.Errorf("query tail = %q, want %q", "&"+query, tc.wantQuery)
			}
			if !ed25519.Verify(pub, []byte(tc.wantPayload), sigBytes(t, got)) {
				t.Errorf("signature does not verify over %q", tc.wantPayload)
			}
		})
	}
}

// TestSanitizeInviteLinkName is the canonicalization matrix the web verifier
// must reproduce byte-for-byte to re-derive the payload from a link.
func TestSanitizeInviteLinkName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain first name", "Mira", "Mira"},
		{"multi-word takes the first token", "Mira Chen", "Mira"},
		{"leading and inner whitespace collapse", "  Ada   Lovelace ", "Ada"},
		{"tab and newline are whitespace", "\tAda\nLovelace", "Ada"},
		{"case is preserved", "mIRA", "mIRA"},
		{"digits stripped from the token", "Mira2", "Mira"},
		{"punctuation stripped", "O'Brien-Smith", "OBrienSmith"},
		{"accents stripped", "José", "Jos"},
		{"email local part survives as letters", "alex.rivera@example.com", "alexriveraexamplecom"},
		{"emoji only omits", "🙂", ""},
		{"digits only omits", "123", ""},
		{"punctuation only omits", "!!!", ""},
		{"empty omits", "", ""},
		{"whitespace only omits", "   \t\n ", ""},
		{"non-Latin script omits", "美佳", ""},
		{"capped at twenty letters", "Abcdefghijklmnopqrstuvwxyz", "Abcdefghijklmnopqrst"},
		{"cap counts letters, not source runes", "A1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0u", "Abcdefghijklmnopqrst"},
		{"second token never leaks past the cap", "Ab Cdefghijklmnopqrstuvwxyz", "Ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeInviteLinkName(tc.in); got != tc.want {
				t.Errorf("sanitizeInviteLinkName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestInviteLinkSigner_ExpiryMatchesRow proves the signed expiry is the
// invite's ACTUAL expires_at in unix seconds, not a re-derived TTL. Sub-second
// precision is dropped, which is the encoding, not a rounding bug.
func TestInviteLinkSigner_ExpiryMatchesRow(t *testing.T) {
	signer, _ := newTestSigner(t)

	cases := []struct {
		name string
		exp  time.Time
	}{
		{"whole second UTC", testExpiry},
		{"sub-second truncates", time.Date(2026, 8, 5, 15, 4, 5, 999_000_000, time.UTC)},
		{"non-UTC zone is still absolute", time.Date(2026, 8, 5, 15, 4, 5, 0, time.FixedZone("PDT", -7*3600))},
		{"already past", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := kParam(t, signer.ShareURL(testCode, tc.exp, testFrom, testTo))
			parts := strings.Split(k, ".")
			if parts[1] != strconv.FormatInt(tc.exp.Unix(), 10) {
				t.Errorf("signed exp = %s, want %d", parts[1], tc.exp.Unix())
			}
		})
	}
}

// TestInviteLinkSigner_Deterministic — Ed25519 is deterministic, so the same
// inputs always yield the same link. A resend changes the link only because it
// changes its inputs, never because signing is random.
func TestInviteLinkSigner_Deterministic(t *testing.T) {
	signer, _ := newTestSigner(t)
	a := signer.ShareURL(testCode, testExpiry, "Alex Rivera", "Mira Chen")
	b := signer.ShareURL(testCode, testExpiry, "Alex Rivera", "Mira Chen")
	if a != b {
		t.Fatalf("signing is not deterministic:\n%s\n%s", a, b)
	}
}

// TestInviteLinkSigner_NoKeyNoURL — a nil signer yields an empty URL rather
// than an unsigned one. The absent-key path must degrade to "no shareUrl", and
// the wire contract says a consumer that finds `code` without `shareUrl` falls
// back to the bare code. An unsigned link would be bounced by the shell and
// look broken to the recipient instead.
func TestInviteLinkSigner_NoKeyNoURL(t *testing.T) {
	var signer *InviteLinkSigner
	if got := signer.ShareURL(testCode, time.Now(), testFrom, testTo); got != "" {
		t.Fatalf("nil signer produced %q, want empty", got)
	}
	if got := signer.PublicKeyBase64(); got != "" {
		t.Fatalf("nil signer public key = %q, want empty", got)
	}
}

// TestInviteLinkSigner_EmptyInputs — a blank code or a zero expiry means the
// caller has nothing to sign (an accepted row, or a row the store did not give
// an expiry). Emitting a link for either would advertise a join URL that cannot
// redeem. An absent NAME is not in this class: it is expected and handled.
func TestInviteLinkSigner_EmptyInputs(t *testing.T) {
	signer, _ := newTestSigner(t)
	if got := signer.ShareURL("", time.Now(), testFrom, testTo); got != "" {
		t.Errorf("empty code produced %q, want empty", got)
	}
	if got := signer.ShareURL(testCode, time.Time{}, testFrom, testTo); got != "" {
		t.Errorf("zero expiry produced %q, want empty", got)
	}
	if got := signer.ShareURL(testCode, testExpiry, "", ""); got == "" {
		t.Error("both names absent produced no URL; it should still sign")
	}
}

// TestNewInviteLinkSigner_SeedValidation — the seed is a P0 secret, so a
// malformed one is a startup failure, never a silently weakened key. The error
// must not echo the value.
func TestNewInviteLinkSigner_SeedValidation(t *testing.T) {
	t.Run("wrong length is refused", func(t *testing.T) {
		if _, err := NewInviteLinkSigner(make([]byte, 16)); err == nil {
			t.Fatal("expected an error for a 16-byte seed")
		}
	})
	t.Run("base64 seed round-trips", func(t *testing.T) {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		seedB64 := base64.StdEncoding.EncodeToString(priv.Seed())
		signer, err := NewInviteLinkSignerFromSeedBase64("  " + seedB64 + "\n")
		if err != nil {
			t.Fatalf("NewInviteLinkSignerFromSeedBase64: %v", err)
		}
		pub, ok := priv.Public().(ed25519.PublicKey)
		if !ok {
			t.Fatal("unexpected public key type")
		}
		if got, want := signer.PublicKeyBase64(), base64.StdEncoding.EncodeToString(pub); got != want {
			t.Errorf("public key = %s, want %s", got, want)
		}
	})
	t.Run("non-base64 is refused without echoing the value", func(t *testing.T) {
		_, err := NewInviteLinkSignerFromSeedBase64("!!!not base64!!!")
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), "not base64!!!") {
			t.Fatalf("error echoes the secret value: %v", err)
		}
	})
	t.Run("empty is refused", func(t *testing.T) {
		if _, err := NewInviteLinkSignerFromSeedBase64(""); err == nil {
			t.Fatal("expected an error for an empty seed")
		}
	})
}

// kParam extracts the `k` query parameter from a share URL.
func kParam(t *testing.T, shareURL string) string {
	t.Helper()
	u, err := url.Parse(shareURL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", shareURL, err)
	}
	k := u.Query().Get("k")
	if k == "" {
		t.Fatalf("no k parameter in %q", shareURL)
	}
	return k
}

// sigBytes extracts and decodes the signature from a share URL's `k`.
func sigBytes(t *testing.T, shareURL string) []byte {
	t.Helper()
	parts := strings.Split(kParam(t, shareURL), ".")
	if len(parts) != 3 {
		t.Fatalf("malformed k in %q", shareURL)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	return sig
}
