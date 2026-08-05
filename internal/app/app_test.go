package app_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mtrenker/pacapulse/internal/app"
	"github.com/mtrenker/pacapulse/internal/config"
	"github.com/mtrenker/pacapulse/internal/httpapi"
	"github.com/mtrenker/pacapulse/internal/logging"
	"github.com/mtrenker/pacapulse/internal/storage"
)

// testConfig loads the real configuration from a fixture environment, so the
// lifecycle test exercises the same values the binary would.
func testConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()

	base := map[string]string{
		config.EnvBaseURL: "http://127.0.0.1:0",
		config.EnvAddr:    "127.0.0.1:0",
		config.EnvDataDir: t.TempDir(),
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

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	service, err := app.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("app.New() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

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

	status, _ := probeBody(t, url)
	return status
}

func probeBody(t *testing.T, url string) (int, string) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
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

// TestStorageIsAReadinessDependency proves the wiring an operator depends on:
// a database the service will not serve from takes the instance out of the load
// balancer without restarting an otherwise healthy process.
func TestStorageIsAReadinessDependency(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// A file that is not a database is what a damaged or misdirected volume
	// looks like on startup.
	if err := os.WriteFile(filepath.Join(dir, storage.DatabaseFileName), []byte("not a database"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var logs bytes.Buffer
	cfg := testConfig(t, map[string]string{config.EnvDataDir: dir})
	_, base, _, _ := startApp(t, cfg, &logs)

	if got := probe(t, base+httpapi.LivenessPath); got != http.StatusOK {
		t.Errorf("liveness status = %d, want %d: the process must stay diagnosable", got, http.StatusOK)
	}
	status, body := probeBody(t, base+httpapi.ReadinessPath)
	if status != http.StatusServiceUnavailable {
		t.Errorf("readiness status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	if !strings.Contains(body, `"`+app.StorageCheck+`"`) {
		t.Errorf("readiness body = %s, want it to name the %q check", body, app.StorageCheck)
	}

	// Neither the probe response nor the operator log may describe the volume.
	for _, forbidden := range []string{dir, storage.DatabaseFileName, "PRAGMA", "SELECT"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("readiness body contains %q: %s", forbidden, body)
		}
		if strings.Contains(logs.String(), forbidden) {
			t.Errorf("log contains %q:\n%s", forbidden, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "database is unavailable") {
		t.Errorf("the failure was not reported to the operator log:\n%s", logs.String())
	}
}

// TestSecondWriterIsRefused covers the failure that must stop startup rather
// than degrade it: another process already owns the database.
func TestSecondWriterIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	owner, err := storage.Open(storage.Options{Dir: dir})
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })

	var logs bytes.Buffer
	cfg := testConfig(t, map[string]string{config.EnvDataDir: dir})
	service, err := app.New(context.Background(), cfg, logging.New(&logs, cfg.LogFormat, cfg.LogLevel))
	if !errors.Is(err, storage.ErrLocked) {
		if service != nil {
			_ = service.Close()
		}
		t.Fatalf("app.New() error = %v, want %v", err, storage.ErrLocked)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the startup failure names the data directory: %v", err)
	}
}

// TestShutdownReleasesTheDatabase proves a terminated process leaves the volume
// free for its replacement.
func TestShutdownReleasesTheDatabase(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	cfg := testConfig(t, map[string]string{config.EnvShutdownTimeout: "5s"})
	service, base, result, cancel := startApp(t, cfg, &logs)

	if got := probe(t, base+httpapi.ReadinessPath); got != http.StatusOK {
		t.Fatalf("readiness status = %d, want %d", got, http.StatusOK)
	}
	if got := service.Storage().Version(); got == 0 {
		t.Error("the database was not migrated during startup")
	}

	cancel()
	if err := <-result; err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	// The lock is gone: a fresh instance can take the same directory.
	next, err := storage.Open(storage.Options{Dir: cfg.DataDir})
	if err != nil {
		t.Fatalf("storage.Open() after shutdown error = %v, want the lock to be released", err)
	}
	if err := next.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
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
	for _, forbidden := range []string{baseURL, "secret-customer", "leaky", "127.0.0.1:0", cfg.DataDir} {
		if strings.Contains(written, forbidden) {
			t.Errorf("log contains %q:\n%s", forbidden, written)
		}
	}
}
