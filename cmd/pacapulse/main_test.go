package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mtrenker/pacapulse/internal/config"
	"github.com/mtrenker/pacapulse/internal/httpapi"
	"github.com/mtrenker/pacapulse/internal/version"
)

func TestVersionFlag(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-version"}, config.Map(nil), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != version.Version() {
		t.Errorf("stdout = %q, want %q", got, version.Version())
	}
}

func TestMissingConfigurationPreventsStartup(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(nil, config.Map(nil), &stdout, &stderr)

	if code != exitConfig {
		t.Fatalf("exit code = %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr.String(), config.EnvBaseURL) {
		t.Errorf("stderr does not name the missing variable: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing before the logger exists", stdout.String())
	}
}

// TestConfigurationErrorHidesValues repeats the redaction guarantee at the
// process boundary, where the message actually reaches an operator's terminal
// or a container log.
func TestConfigurationErrorHidesValues(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-value-that-must-never-be-logged"

	var stdout, stderr bytes.Buffer
	code := run(nil, config.Map(map[string]string{
		config.EnvBaseURL:  "https://pulse.example.com",
		config.EnvLogLevel: secret,
	}), &stdout, &stderr)

	if code != exitConfig {
		t.Fatalf("exit code = %d, want %d", code, exitConfig)
	}
	if strings.Contains(stderr.String(), secret) {
		t.Errorf("stderr leaked the configured value: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), config.EnvLogLevel) {
		t.Errorf("stderr does not name the invalid variable: %q", stderr.String())
	}
}

func TestUnexpectedArgumentIsRejected(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"serve"}, config.Map(nil), &stdout, &stderr); code != exitConfig {
		t.Fatalf("exit code = %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr.String(), "unexpected argument") {
		t.Errorf("stderr = %q, want it to reject the argument", stderr.String())
	}
}

func TestUnknownFlagIsRejected(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-nope"}, config.Map(nil), &stdout, &stderr); code != exitConfig {
		t.Fatalf("exit code = %d, want %d", code, exitConfig)
	}
}

// TestHealthCheckFlag exercises the container health check against a stand-in
// readiness endpoint, including the unready case the OCI HEALTHCHECK relies on.
func TestHealthCheckFlag(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status int
		want   int
	}{
		"ready":   {http.StatusOK, exitOK},
		"unready": {http.StatusServiceUnavailable, exitFailure},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			requested := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case requested <- r.URL.Path:
				default:
				}
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)

			var stdout, stderr bytes.Buffer
			env := config.Map(map[string]string{
				config.EnvAddr: strings.TrimPrefix(server.URL, "http://"),
			})
			if code := run([]string{"-health-check"}, env, &stdout, &stderr); code != tc.want {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tc.want, stderr.String())
			}
			select {
			case got := <-requested:
				if got != httpapi.ReadinessPath {
					t.Errorf("probed path = %q, want %q", got, httpapi.ReadinessPath)
				}
			default:
				t.Error("the health check did not reach the readiness endpoint")
			}
		})
	}
}

func TestHealthCheckFailsWhenNothingIsListening(t *testing.T) {
	t.Parallel()

	// Port 1 on loopback is reserved and not bindable by an unprivileged test,
	// so the connection is refused rather than answered.
	env := config.Map(map[string]string{config.EnvAddr: "127.0.0.1:1"})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-health-check"}, env, &stdout, &stderr); code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "health check request failed") {
		t.Errorf("stderr = %q, want a health check failure", stderr.String())
	}
}

func TestHealthCheckRejectsInvalidListenAddress(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	env := config.Map(map[string]string{config.EnvAddr: "nonsense"})
	if code := run([]string{"-health-check"}, env, &stdout, &stderr); code != exitConfig {
		t.Fatalf("exit code = %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr.String(), config.EnvAddr) {
		t.Errorf("stderr does not name the invalid variable: %q", stderr.String())
	}
}
