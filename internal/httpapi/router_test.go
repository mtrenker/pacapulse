package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mtrenker/pacapulse/internal/beacon"
	"github.com/mtrenker/pacapulse/internal/health"
	"github.com/mtrenker/pacapulse/internal/httpapi"
	"github.com/mtrenker/pacapulse/internal/logging"
)

type probeBody struct {
	Status string   `json:"status"`
	Failed []string `json:"failed"`
}

// newRouter returns a router plus the buffer its logs are written to. Debug
// level is used so that every record the service can emit is observable.
func newRouter(t *testing.T, registry *health.Registry) (http.Handler, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	logger := logging.New(&logs, "json", slog.LevelDebug)
	return httpapi.NewRouter(httpapi.Options{Logger: logger, Health: registry}), &logs
}

// get issues a GET against the router and decodes the probe response.
func get(t *testing.T, handler http.Handler, target string) (*httptest.ResponseRecorder, probeBody) {
	t.Helper()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))

	var body probeBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s response: %v", target, err)
	}
	return recorder, body
}

func TestLivenessReportsProcessIsRunning(t *testing.T) {
	t.Parallel()

	handler, _ := newRouter(t, health.NewRegistry())
	resp, body := get(t, handler, httpapi.LivenessPath)

	if resp.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	if body.Status != "alive" {
		t.Errorf("status field = %q, want %q", body.Status, "alive")
	}
	if got := resp.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
	if got := resp.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", got)
	}
}

// TestLivenessIgnoresDependencies pins the distinction between the two probes:
// a failing dependency must make the instance unready, not make Kubernetes
// restart an otherwise healthy process.
func TestLivenessIgnoresDependencies(t *testing.T) {
	t.Parallel()

	registry := health.NewRegistry()
	registry.Register("storage", func(context.Context) error { return errors.New("down") })
	handler, _ := newRouter(t, registry)

	if resp, _ := get(t, handler, httpapi.LivenessPath); resp.Code != http.StatusOK {
		t.Errorf("liveness status = %d with a failing dependency, want %d", resp.Code, http.StatusOK)
	}
	if resp, _ := get(t, handler, httpapi.ReadinessPath); resp.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness status = %d with a failing dependency, want %d", resp.Code, http.StatusServiceUnavailable)
	}
}

func TestReadinessWithoutChecksIsReady(t *testing.T) {
	t.Parallel()

	handler, _ := newRouter(t, health.NewRegistry())
	resp, body := get(t, handler, httpapi.ReadinessPath)

	if resp.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	if body.Status != "ready" {
		t.Errorf("status field = %q, want %q", body.Status, "ready")
	}
}

// TestReadinessResponseHidesCheckErrors keeps dependency detail out of the
// least access-controlled response the service has. The name identifies the
// check; the reason belongs in the operator-only log.
func TestReadinessResponseHidesCheckErrors(t *testing.T) {
	t.Parallel()

	const detail = "dial tcp 10.9.8.7:5432: connection refused"
	registry := health.NewRegistry()
	registry.Register("storage", func(context.Context) error { return errors.New(detail) })

	handler, logs := newRouter(t, registry)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, httpapi.ReadinessPath, nil))

	raw := recorder.Body.String()
	if strings.Contains(raw, "10.9.8.7") || strings.Contains(raw, "connection refused") {
		t.Errorf("readiness response leaked dependency detail: %s", raw)
	}
	if !strings.Contains(raw, "storage") {
		t.Errorf("readiness response omits the failing check name: %s", raw)
	}
	if !strings.Contains(logs.String(), detail) {
		t.Errorf("readiness failure detail is missing from the log: %s", logs.String())
	}
}

func TestReadinessReportsDrainingAfterTerminationBegins(t *testing.T) {
	t.Parallel()

	registry := health.NewRegistry()
	handler, _ := newRouter(t, registry)

	registry.BeginDrain()

	resp, body := get(t, handler, httpapi.ReadinessPath)
	if resp.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.Code, http.StatusServiceUnavailable)
	}
	if body.Status != "draining" {
		t.Errorf("status field = %q, want %q", body.Status, "draining")
	}

	if resp, _ := get(t, handler, httpapi.LivenessPath); resp.Code != http.StatusOK {
		t.Errorf("liveness status = %d while draining, want %d", resp.Code, http.StatusOK)
	}
}

func TestRouterRejectsUnknownRoutesAndMethods(t *testing.T) {
	t.Parallel()

	handler, _ := newRouter(t, health.NewRegistry())

	tests := map[string]struct {
		method string
		target string
		want   int
	}{
		"unknown path":        {http.MethodGet, "/", http.StatusNotFound},
		"post to liveness":    {http.MethodPost, httpapi.LivenessPath, http.StatusMethodNotAllowed},
		"delete to readiness": {http.MethodDelete, httpapi.ReadinessPath, http.StatusMethodNotAllowed},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.target, nil))
			if recorder.Code != tc.want {
				t.Errorf("status = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}

// TestLogsExcludeRequestData is the guard behind the privacy acceptance
// criterion. Everything below is either visitor-supplied or identifying, and
// none of it may appear in the logs at any level.
func TestLogsExcludeRequestData(t *testing.T) {
	t.Parallel()

	const (
		queryValue  = "utm-secret-campaign"
		cookieValue = "session-identifier"
		authValue   = "Bearer super-secret-token"
		agentValue  = "Mozilla/5.0 (very identifying)"
		refererURL  = "https://referrer.example.com/private/page"
		clientIP    = "203.0.113.42"
		unknownPath = "/visitor/supplied/path"
	)

	handler, logs := newRouter(t, health.NewRegistry())

	for _, target := range []string{
		httpapi.LivenessPath + "?q=" + queryValue,
		httpapi.ReadinessPath + "?q=" + queryValue,
		unknownPath + "?q=" + queryValue,
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Cookie", "sid="+cookieValue)
		req.Header.Set("Authorization", authValue)
		req.Header.Set("User-Agent", agentValue)
		req.Header.Set("Referer", refererURL)
		req.RemoteAddr = clientIP + ":54321"

		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	written := logs.String()
	if written == "" {
		t.Fatal("no log records were written; the privacy assertions would be vacuous")
	}
	for _, forbidden := range []string{
		queryValue, cookieValue, authValue, agentValue, refererURL, clientIP, unknownPath,
	} {
		if strings.Contains(written, forbidden) {
			t.Errorf("log contains request data %q:\n%s", forbidden, written)
		}
	}
	// The route pattern comes from the routing table, so it is safe to log and
	// is what makes the records useful at all.
	if !strings.Contains(written, httpapi.LivenessPath) {
		t.Errorf("log omits the matched route pattern:\n%s", written)
	}
	if !strings.Contains(written, "unmatched") {
		t.Errorf("log omits the placeholder for unrouted requests:\n%s", written)
	}
}

// TestSuccessfulRequestsAreQuietByDefault keeps Kubernetes probe traffic out of
// the default log while leaving server errors visible.
func TestSuccessfulRequestsAreQuietByDefault(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := logging.New(&logs, "json", slog.LevelInfo)
	handler := httpapi.NewRouter(httpapi.Options{Logger: logger, Health: health.NewRegistry()})

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, httpapi.LivenessPath, nil))

	if strings.Contains(logs.String(), "request served") {
		t.Errorf("successful probe was logged at the default level:\n%s", logs.String())
	}
}

// TestBeaconAssetIsRoutedWithoutLoggingRequestData registers the real asset
// handler on the router. The route pattern is a constant, so the privacy
// guarantee the middleware provides has to hold here too.
func TestBeaconAssetIsRoutedWithoutLoggingRequestData(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := logging.New(&logs, "json", slog.LevelDebug)
	handler := httpapi.NewRouter(httpapi.Options{
		Logger: logger,
		Health: health.NewRegistry(),
		Beacon: beacon.NewHandler(),
	})

	req := httptest.NewRequest(http.MethodGet, httpapi.BeaconPath, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Referer", "https://tracked-site.example/private/page")
	req.Header.Set("User-Agent", "Mozilla/5.0 (very identifying)")
	req.RemoteAddr = "203.0.113.42:54321"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}

	written := logs.String()
	if !strings.Contains(written, httpapi.BeaconPath) {
		t.Errorf("log omits the matched route pattern:\n%s", written)
	}
	for _, forbidden := range []string{"tracked-site.example", "Mozilla/5.0", "203.0.113.42"} {
		if strings.Contains(written, forbidden) {
			t.Errorf("log contains request data %q:\n%s", forbidden, written)
		}
	}
}

// TestBeaconRouteIsOmittedWhenUnwired keeps isolated probe tests lightweight
// and matches how the collector route behaves.
func TestBeaconRouteIsOmittedWhenUnwired(t *testing.T) {
	t.Parallel()

	handler, _ := newRouter(t, health.NewRegistry())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, httpapi.BeaconPath, nil))

	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}
