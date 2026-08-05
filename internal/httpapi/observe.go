package httpapi

import (
	"log/slog"
	"net/http"
	"time"
)

// unmatchedRoute labels requests that no route claimed. Reporting the literal
// requested path here would put attacker- and visitor-supplied strings into the
// logs, which is exactly what this middleware exists to avoid.
const unmatchedRoute = "unmatched"

// observe emits one record per request.
//
// It logs the request method, the matched route pattern, the response status,
// and the duration — nothing else. In particular it never reads the raw path,
// the query string, request headers, cookies, or r.RemoteAddr, so no
// visitor-supplied or identifying data can reach the logs through this path.
// The route pattern is safe because it comes from the routing table, not from
// the request.
//
// Successful requests log at debug level: readiness and liveness probes run
// every few seconds and would otherwise dominate the log at the default level.
// Server errors log at error level so they remain visible.
func observe(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		route := r.Pattern
		if route == "" {
			route = unmatchedRoute
		}
		level := slog.LevelDebug
		if recorder.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		logger.LogAttrs(r.Context(), level, "request served",
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.Int("status", recorder.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if !s.wroteHeader {
		s.status = status
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying writer to http.ResponseController so that
// wrapping does not remove flushing or deadline control from later handlers.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
