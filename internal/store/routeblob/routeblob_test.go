package routeblob

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/tnando/my-robo-taxi-telemetry/internal/cryptox"
)

// newTestEncryptor mints a fresh AES-256-GCM Encryptor backed by a
// random key. The key never leaves the test process so a leaked log
// line can't compromise production data.
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

func TestEncryptDecryptNavRoute_Roundtrip(t *testing.T) {
	enc := newTestEncryptor(t)
	tests := []struct {
		name   string
		coords []NavRouteCoordinate
	}{
		{name: "single-point", coords: []NavRouteCoordinate{{-96.80, 33.10}}},
		{name: "multi-point", coords: []NavRouteCoordinate{
			{-96.80, 33.10}, {-96.81, 33.11}, {-96.82, 33.12},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct, err := EncryptNavRoute(tt.coords, enc)
			if err != nil {
				t.Fatalf("EncryptNavRoute: %v", err)
			}
			if ct == "" {
				t.Fatalf("EncryptNavRoute returned empty for non-empty input")
			}
			got, err := DecryptNavRoute(ct, enc)
			if err != nil {
				t.Fatalf("DecryptNavRoute: %v", err)
			}
			if len(got) != len(tt.coords) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.coords))
			}
			for i := range got {
				if got[i] != tt.coords[i] {
					t.Errorf("[%d] = %v, want %v", i, got[i], tt.coords[i])
				}
			}
		})
	}
}

func TestEncryptNavRoute_EmptyInput(t *testing.T) {
	enc := newTestEncryptor(t)
	tests := []struct {
		name string
		in   []NavRouteCoordinate
	}{
		{name: "nil", in: nil},
		{name: "empty", in: []NavRouteCoordinate{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct, err := EncryptNavRoute(tt.in, enc)
			if err != nil {
				t.Fatalf("EncryptNavRoute: %v", err)
			}
			if ct != "" {
				t.Errorf("EncryptNavRoute(empty) = %q, want \"\"", ct)
			}
		})
	}
}

func TestDecryptNavRoute_EmptyCiphertext(t *testing.T) {
	enc := newTestEncryptor(t)
	got, err := DecryptNavRoute("", enc)
	if err != nil {
		t.Fatalf("DecryptNavRoute(\"\"): %v", err)
	}
	if got != nil {
		t.Errorf("DecryptNavRoute(\"\") = %v, want nil", got)
	}
}

// TestDecryptNavRoute_NonArrayShape verifies that ciphertext whose
// plaintext is not a JSON array is reported as an error so the caller
// falls back to plaintext rather than returning nil silently.
func TestDecryptNavRoute_NonArrayShape(t *testing.T) {
	enc := newTestEncryptor(t)
	// Encrypt a JSON object instead of an array.
	ct, err := enc.EncryptString(`{"oops":"not an array"}`)
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	if _, err := DecryptNavRoute(ct, enc); err == nil {
		t.Fatal("expected unmarshal error on non-array shape, got nil")
	}
}

// TestDecryptNavRoute_DecryptFailureReturnsError verifies a malformed
// ciphertext (truncated, wrong key, garbage) propagates a non-nil
// error so the caller's fallback path runs.
func TestDecryptNavRoute_DecryptFailureReturnsError(t *testing.T) {
	enc := newTestEncryptor(t)
	tests := []struct {
		name       string
		ciphertext string
	}{
		{name: "garbage", ciphertext: "not-base64-at-all"},
		{name: "valid-base64-not-ciphertext", ciphertext: base64.StdEncoding.EncodeToString([]byte("hello"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecryptNavRoute(tt.ciphertext, enc); err == nil {
				t.Fatalf("expected decrypt error, got nil")
			}
		})
	}
}

// TestDecryptNavRoute_WrongKey verifies a ciphertext sealed under one
// key cannot be opened by another. Cross-key contamination would silently
// surface garbage to consumers; the auth-tag check inside cryptox is the
// load-bearing guard and we want it covered here.
func TestDecryptNavRoute_WrongKey(t *testing.T) {
	encA := newTestEncryptor(t)
	encB := newTestEncryptor(t) // independent random key

	ct, err := EncryptNavRoute([]NavRouteCoordinate{{-1, 2}}, encA)
	if err != nil {
		t.Fatalf("EncryptNavRoute: %v", err)
	}
	if _, err := DecryptNavRoute(ct, encB); err == nil {
		t.Fatal("expected decrypt error with wrong key, got nil")
	}
}

func TestEncryptDecryptRoutePoints_Roundtrip(t *testing.T) {
	enc := newTestEncryptor(t)
	pts := []RoutePoint{
		{Latitude: 33.10, Longitude: -96.80, Speed: 35, Heading: 90, Timestamp: "2026-05-09T12:00:00Z"},
		{Latitude: 33.11, Longitude: -96.81, Speed: 40, Heading: 91, Timestamp: "2026-05-09T12:00:05Z"},
	}
	ct, err := EncryptRoutePoints(pts, enc)
	if err != nil {
		t.Fatalf("EncryptRoutePoints: %v", err)
	}
	got, err := DecryptRoutePoints(ct, enc)
	if err != nil {
		t.Fatalf("DecryptRoutePoints: %v", err)
	}
	if len(got) != len(pts) {
		t.Fatalf("len = %d, want %d", len(got), len(pts))
	}
	for i := range got {
		if got[i] != pts[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], pts[i])
		}
	}
}

func TestEncryptRoutePoints_EmptyInput(t *testing.T) {
	enc := newTestEncryptor(t)
	ct, err := EncryptRoutePoints(nil, enc)
	if err != nil {
		t.Fatalf("EncryptRoutePoints: %v", err)
	}
	if ct != "" {
		t.Errorf("EncryptRoutePoints(nil) = %q, want \"\"", ct)
	}
}

func TestDecryptRoutePoints_NonArrayShape(t *testing.T) {
	enc := newTestEncryptor(t)
	ct, err := enc.EncryptString(`{"oops":"not an array"}`)
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	if _, err := DecryptRoutePoints(ct, enc); err == nil {
		t.Fatal("expected unmarshal error on non-array shape, got nil")
	}
}

// TestEncryptJSONBytes_AbsentSentinels verifies the empty-input shapes
// that map to a NULL *Enc column rather than an encrypted blob:
// nil bytes, empty bytes, JSON null, JSON empty array, whitespace.
func TestEncryptJSONBytes_AbsentSentinels(t *testing.T) {
	enc := newTestEncryptor(t)
	tests := []struct {
		name string
		in   []byte
	}{
		{name: "nil", in: nil},
		{name: "empty", in: []byte{}},
		{name: "json-null", in: []byte("null")},
		{name: "json-empty-array", in: []byte("[]")},
		{name: "padded-empty-array", in: []byte("  []  \n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct, err := EncryptJSONBytes(tt.in, enc)
			if err != nil {
				t.Fatalf("EncryptJSONBytes: %v", err)
			}
			if ct != "" {
				t.Errorf("EncryptJSONBytes(absent=%q) = %q, want \"\"", tt.in, ct)
			}
		})
	}
}

// TestEncryptJSONBytes_PopulatedRoundtripsViaDecryptJSONBytes is the
// happy-path the Vehicle write/read path exercises.
func TestEncryptJSONBytes_PopulatedRoundtripsViaDecryptJSONBytes(t *testing.T) {
	enc := newTestEncryptor(t)
	raw := []byte(`[[-96.80,33.10],[-96.81,33.11]]`)

	ct, err := EncryptJSONBytes(raw, enc)
	if err != nil {
		t.Fatalf("EncryptJSONBytes: %v", err)
	}
	if ct == "" {
		t.Fatalf("EncryptJSONBytes returned empty for populated input")
	}
	got, err := DecryptJSONBytes(ct, enc)
	if err != nil {
		t.Fatalf("DecryptJSONBytes: %v", err)
	}
	if string(got) != string(raw) {
		t.Errorf("DecryptJSONBytes round-trip = %q, want %q", got, raw)
	}
}

// TestDecryptJSONBytes_DecryptFailureReturnsError covers the wrapped
// error path so callers can react to corruption.
func TestDecryptJSONBytes_DecryptFailureReturnsError(t *testing.T) {
	enc := newTestEncryptor(t)
	if _, err := DecryptJSONBytes("not-base64", enc); err == nil {
		t.Fatal("expected decrypt error, got nil")
	}
	// Empty round-trips silently for empty.
	got, err := DecryptJSONBytes("", enc)
	if err != nil {
		t.Fatalf("DecryptJSONBytes(\"\"): %v", err)
	}
	if got != nil {
		t.Errorf("DecryptJSONBytes(\"\") = %v, want nil", got)
	}
}

// TestErrorWrappingPreservesUnderlying verifies the package's wrapped
// errors are matchable via errors.Is so the caller can distinguish
// crypto failures from JSON failures if it ever needs to.
func TestErrorWrappingPreservesUnderlying(t *testing.T) {
	enc := newTestEncryptor(t)
	_, err := DecryptNavRoute("not-base64", enc)
	if err == nil {
		t.Fatal("expected error")
	}
	// Should be wrapped — fmt.Errorf("...: %w", ...) keeps Unwrap intact.
	if !errors.Is(err, errors.Unwrap(err)) {
		t.Errorf("error not wrapped via %%w: %v", err)
	}
	if !strings.Contains(err.Error(), "routeblob.DecryptNavRoute") {
		t.Errorf("missing context prefix in wrapped error: %v", err)
	}
}
