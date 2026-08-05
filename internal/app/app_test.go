package app_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mtrenker/pacapulse/internal/app"
	"github.com/mtrenker/pacapulse/internal/config"
	"github.com/mtrenker/pacapulse/internal/httpapi"
	"github.com/mtrenker/pacapulse/internal/logging"
)

// testConfig loads the real configuration from a fixture environment, so the
// lifecycle test exercises the same values the binary would.
func testConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()

	base := map[string]string{
		config.EnvBaseURL: "http://127.0.0.1:0",
		config.EnvAddr:    "127.0.0.1:0",
	}
	for k, v := range env {
		base[k] = v
	}
	cfg, err := config.Load(config.Map(base))
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	return cfg
}

// startApp runs the assembled service and returns it with its base URL.
func startApp(t *testing.T, cfg config.Config, logs io.Writer) (*app.App, string, <-chan error, context.CancelFunc) {
	t.Helper()

	logger := logging.New(logs, cfg.LogFormat, cfg.LogLevel)
	service := app.New(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	result := make(chan error, 1)
	go func() { result <- service.Run(ctx) }()

	select {
	case <-service.Started():
	case err := <-result:
		t.Fatalf("service exited before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("service did not start within 5s")
	}

	return service, "http://" + service.Addr().String(), result, cancel
}

func probe(t *testing.T, url string) int {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestServiceLifecycle covers the whole baseline contract end to end: the
// assembled service listens, answers both probes, stops advertising readiness
// once termination begins, drains, and exits cleanly inside the grace period.
func TestServiceLifecycle(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	cfg := testConfig(t, map[string]string{config.EnvShutdownTimeout: "5s"})
	service, base, result, cancel := startApp(t, cfg, &logs)

	if got := probe(t, base+httpapi.LivenessPath); got != http.StatusOK {
		t.Errorf("liveness status = %d, want %d", got, http.StatusOK)
	}
	if got := probe(t, base+httpapi.ReadinessPath); got != http.StatusOK {
		t.Errorf("readiness status = %d, want %d", got, http.StatusOK)
	}

	// A dependency registered later flips readiness without affecting liveness.
	service.Health().Register("storage", func(context.Context) error { return errors.New("not wired yet") })
	if got := probe(t, base+httpapi.ReadinessPath); got != http.StatusServiceUnavailable {
		t.Errorf("readiness status = %d with a failing dependency, want %d", got, http.StatusServiceUnavailable)
	}
	if got := probe(t, base+httpapi.LivenessPath); got != http.StatusOK {
		t.Errorf("liveness status = %d with a failing dependency, want %d", got, http.StatusOK)
	}

	start := time.Now()
	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(cfg.ShutdownTimeout + 5*time.Second):
		t.Fatal("service did not terminate within the grace period")
	}
	if elapsed := time.Since(start); elapsed > cfg.ShutdownTimeout {
		t.Errorf("shutdown took %s, want at most the %s grace period", elapsed, cfg.ShutdownTimeout)
	}
	if !service.Health().Draining() {
		t.Error("readiness was not drained during termination")
	}
}

// TestReadinessFailsWhileDraining proves that a terminating instance reports
// itself unready to Kubernetes before it stops serving.
func TestReadinessFailsWhileDraining(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	service, base, _, _ := startApp(t, testConfig(t, nil), &logs)

	service.Health().BeginDrain()

	if got := probe(t, base+httpapi.ReadinessPath); got != http.StatusServiceUnavailable {
		t.Errorf("readiness status = %d while draining, want %d", got, http.StatusServiceUnavailable)
	}
	if got := probe(t, base+httpapi.LivenessPath); got != http.StatusOK {
		t.Errorf("liveness status = %d while draining, want %d", got, http.StatusOK)
	}
}

// TestLogsExcludeConfiguredSecrets checks the running service, not just the
// router: no configured value beyond the bound listen address may appear in the
// logs, at any level.
func TestLogsExcludeConfiguredSecrets(t *testing.T) {
	t.Parallel()

	const baseURL = "https://pulse.secret-customer.example.com"

	var logs bytes.Buffer
	cfg := testConfig(t, map[string]string{
		config.EnvBaseURL:  baseURL,
		config.EnvLogLevel: "debug",
	})
	_, base, result, cancel := startApp(t, cfg, &logs)

	req, err := http.NewRequest(http.MethodGet, base+httpapi.LivenessPath+"?token=leaky", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer leaky-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	_ = resp.Body.Close()

	cancel()
	if err := <-result; err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	written := logs.String()
	if written == "" {
		t.Fatal("no log records were written; the privacy assertions would be vacuous")
	}
	for _, forbidden := range []string{baseURL, "secret-customer", "leaky", "127.0.0.1:0"} {
		if strings.Contains(written, forbidden) {
			t.Errorf("log contains %q:\n%s", forbidden, written)
		}
	}
}
