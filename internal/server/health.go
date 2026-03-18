package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// ReadinessChecker tests whether a dependency is ready to serve traffic.
// store.DB satisfies this interface via its Ping method.
type ReadinessChecker interface {
	Ping(ctx context.Context) error
}

// healthResponse is the JSON body returned by health check endpoints.
type healthResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// handleHealthz returns 200 immediately. Kubernetes uses this as a liveness
// probe — if it fails, the pod is killed and restarted.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// handleReadyz pings the database and returns 200 if ready or 503 if not.
// Kubernetes uses this as a readiness probe — if it fails, the pod is
// removed from the Service's endpoint list until it passes again.
func handleReadyz(checker ReadinessChecker, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const timeout = 2 * time.Second
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		if err := checker.Ping(ctx); err != nil {
			logger.Warn("readiness check failed", slog.String("error", err.Error()))
			writeJSON(w, http.StatusServiceUnavailable, healthResponse{
				Status: "not ready",
				Error:  err.Error(),
			})
			return
		}

		writeJSON(w, http.StatusOK, healthResponse{Status: "ready"})
	}
}

// writeJSON marshals v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Response already partially written; log but don't attempt recovery.
		slog.Default().Error("failed to write JSON response", slog.String("error", err.Error()))
	}
}
