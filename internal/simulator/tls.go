package simulator

import (
	"crypto/tls"
	"net/http"
)

// tlsHTTPClient creates an http.Client with the provided TLS configuration.
// Used by Vehicle.dial to establish mTLS WebSocket connections.
// Forces HTTP/1.1 because WebSocket upgrade is not supported over HTTP/2.
func tlsHTTPClient(cfg *tls.Config) *http.Client {
	tlsCfg := cfg.Clone()
	tlsCfg.NextProtos = []string{"http/1.1"}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
			ForceAttemptHTTP2: false,
		},
	}
}
