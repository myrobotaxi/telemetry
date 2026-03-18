package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"testing"
	"time"
)

func TestExtractVIN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     func(t *testing.T) *http.Request
		wantVIN string
		wantErr error
	}{
		{
			name: "valid cert with VIN in CN",
			req: func(t *testing.T) *http.Request {
				t.Helper()
				return requestWithCertCN(t, testVIN)
			},
			wantVIN: testVIN,
		},
		{
			name: "no TLS",
			req: func(t *testing.T) *http.Request {
				t.Helper()
				r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
				return r
			},
			wantErr: ErrNoCertificate,
		},
		{
			name: "TLS but no peer certs",
			req: func(t *testing.T) *http.Request {
				t.Helper()
				r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
				r.TLS = &tls.ConnectionState{}
				return r
			},
			wantErr: ErrNoCertificate,
		},
		{
			name: "cert with empty CN",
			req: func(t *testing.T) *http.Request {
				t.Helper()
				return requestWithCertCN(t, "")
			},
			wantErr: ErrEmptyCertVIN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			vin, err := extractVIN(tt.req(t))

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("extractVIN() error = %v, want %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("extractVIN() unexpected error: %v", err)
			}
			if vin != tt.wantVIN {
				t.Errorf("extractVIN() = %q, want %q", vin, tt.wantVIN)
			}
		})
	}
}

func TestRedactVIN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		vin  string
		want string
	}{
		{
			name: "standard 17-char VIN",
			vin:  "5YJ3E7EB2NF000001",
			want: "***0001",
		},
		{
			name: "short VIN",
			vin:  "ABCD",
			want: "ABCD",
		},
		{
			name: "3 chars",
			vin:  "ABC",
			want: "ABC",
		},
		{
			name: "5 chars",
			vin:  "ABCDE",
			want: "***BCDE",
		},
		{
			name: "empty",
			vin:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := redactVIN(tt.vin)
			if got != tt.want {
				t.Errorf("redactVIN(%q) = %q, want %q", tt.vin, got, tt.want)
			}
		})
	}
}

// requestWithCertCN creates an http.Request with a TLS connection state
// containing a self-signed certificate with the given Common Name.
func requestWithCertCN(t *testing.T, cn string) *http.Request {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
	}
	return r
}
