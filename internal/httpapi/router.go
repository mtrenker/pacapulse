// Package httpapi wires the HTTP surface of the service.
//
// The baseline exposes probe endpoints only. Later issues add the collector,
// dashboard, and administration routes to the same router.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/mtrenker/pacapulse/internal/health"
)

// Probe paths. They are stable: Kubernetes manifests and the container health
// check refer to them.
const (
	LivenessPath  = "/healthz"
	ReadinessPath = "/readyz"
)

// DefaultCheckTimeout bounds readiness evaluation so a stuck dependency cannot
// hold a probe connection open until the server write timeout.
const DefaultCheckTimeout = 2 * time.Second

// Options configures the router.
type Options struct {
	// Logger receives request and readiness records. Required.
	Logger *slog.Logger
	// Health supplies readiness state. Required.
	Health *health.Registry
	// CheckTimeout bounds readiness evaluation. Zero selects
	// DefaultCheckTimeout.
	CheckTimeout time.Duration
}

// NewRouter returns the service handler.
func NewRouter(opts Options) http.Handler {
	if opts.CheckTimeout <= 0 {
		opts.CheckTimeout = DefaultCheckTimeout
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+LivenessPath, liveness)
	mux.HandleFunc("GET "+ReadinessPath, readiness(opts.Logger, opts.Health, opts.CheckTimeout))

	return observe(opts.Logger, mux)
}

// probeResponse is the body of both probe endpoints. It carries a status word
// and, when unready, the names of the failing checks. Check error messages are
// deliberately excluded: they can contain hostnames or connection details, and
// probe responses are the least controlled output the service has.
type probeResponse struct {
	Status string   `json:"status"`
	Failed []string `json:"failed,omitempty"`
}

// liveness reports that the process is running and its HTTP server is serving.
// It performs no dependency checks: a failing dependency must not cause
// Kubernetes to restart an otherwise healthy process.
func liveness(w http.ResponseWriter, _ *http.Request) {
	writeProbe(w, http.StatusOK, probeResponse{Status: "alive"})
}

// readiness reports whether this instance should receive traffic.
func readiness(logger *slog.Logger, registry *health.Registry, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		status := registry.Readiness(ctx)
		switch {
		case status.Draining:
			writeProbe(w, http.StatusServiceUnavailable, probeResponse{Status: "draining"})
		case status.Ready:
			writeProbe(w, http.StatusOK, probeResponse{Status: "ready"})
		default:
			names := make([]string, 0, len(status.Failed))
			for _, failure := range status.Failed {
				names = append(names, failure.Name)
				// The error text stays in the logs, which are operator-only.
				logger.WarnContext(ctx, "readiness check failed", "check", failure.Name, "error", failure.Err)
			}
			writeProbe(w, http.StatusServiceUnavailable, probeResponse{Status: "unready", Failed: names})
		}
	}
}

func writeProbe(w http.ResponseWriter, status int, body probeResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Probe results describe a single moment; a cached response would report
	// a drained instance as ready.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// The body is a fixed-shape struct, so encoding cannot fail for reasons
	// worth handling; a broken connection is already reflected in the logs.
	_ = json.NewEncoder(w).Encode(body)
}
