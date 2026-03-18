package telemetry

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrNoCertificate is returned when a request has no client certificate.
var ErrNoCertificate = errors.New("no client certificate presented")

// ErrEmptyCertVIN is returned when a client certificate has an empty
// Common Name (CN) where the VIN is expected.
var ErrEmptyCertVIN = errors.New("client certificate CN is empty")

// extractVIN extracts the Vehicle Identification Number from the mTLS
// client certificate's Common Name (CN). Tesla Fleet Telemetry uses the
// vehicle's VIN as the certificate CN.
func extractVIN(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", fmt.Errorf("extractVIN: %w", ErrNoCertificate)
	}

	vin := r.TLS.PeerCertificates[0].Subject.CommonName
	if vin == "" {
		return "", fmt.Errorf("extractVIN: %w", ErrEmptyCertVIN)
	}

	return vin, nil
}

// redactVIN returns a redacted version of a VIN suitable for logging.
// It shows only the last 4 characters, replacing the rest with asterisks.
// If the VIN is shorter than 4 characters, it is returned as-is.
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}
