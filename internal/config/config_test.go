package config_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mtrenker/pacapulse/internal/config"
)

// minimal is the smallest environment that starts the service.
func minimal() map[string]string {
	return map[string]string{config.EnvBaseURL: "https://pulse.example.com"}
}

func TestLoadAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(config.Map(minimal()))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.Addr != config.DefaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, config.DefaultAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
	if cfg.LogFormat != config.FormatJSON {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, config.FormatJSON)
	}
	if cfg.ShutdownTimeout != config.DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, config.DefaultShutdownTimeout)
	}
	if cfg.ReadHeaderTimeout <= 0 || cfg.ReadTimeout <= 0 || cfg.WriteTimeout <= 0 || cfg.IdleTimeout <= 0 {
		t.Errorf("HTTP timeouts must all be bounded, got %+v", cfg)
	}
	if cfg.MaxHeaderBytes <= 0 {
		t.Errorf("MaxHeaderBytes = %d, want a positive bound", cfg.MaxHeaderBytes)
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(config.Map(map[string]string{
		config.EnvAddr:            "127.0.0.1:9000",
		config.EnvBaseURL:         "https://pulse.example.com",
		config.EnvLogLevel:        "DEBUG",
		config.EnvLogFormat:       "Text",
		config.EnvShutdownTimeout: "5s",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.Addr != "127.0.0.1:9000" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "127.0.0.1:9000")
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
	if cfg.LogFormat != config.FormatText {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, config.FormatText)
	}
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, 5*time.Second)
	}
}

func TestLoadTreatsBlankValuesAsUnset(t *testing.T) {
	t.Parallel()

	// A Kubernetes ConfigMap key with no value arrives as an empty string; it
	// should fall back to the default rather than fail validation.
	cfg, err := config.Load(config.Map(map[string]string{
		config.EnvBaseURL:  "  https://pulse.example.com  ",
		config.EnvAddr:     "   ",
		config.EnvLogLevel: "",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Addr != config.DefaultAddr {
		t.Errorf("Addr = %q, want the default %q", cfg.Addr, config.DefaultAddr)
	}
	if cfg.BaseURL != "https://pulse.example.com" {
		t.Errorf("BaseURL = %q, want the trimmed value", cfg.BaseURL)
	}
}

func TestLoadNormalisesBaseURL(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"https://pulse.example.com":           "https://pulse.example.com",
		"https://pulse.example.com/":          "https://pulse.example.com",
		"https://pulse.example.com/analytics": "https://pulse.example.com/analytics",
		"http://localhost:8080/":              "http://localhost:8080",
	}

	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(config.Map(map[string]string{config.EnvBaseURL: input}))
			if err != nil {
				t.Fatalf("Load(%q) error = %v, want nil", input, err)
			}
			if cfg.BaseURL != want {
				t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, want)
			}
		})
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		env      map[string]string
		variable string
	}{
		"missing base URL": {
			env:      map[string]string{},
			variable: config.EnvBaseURL,
		},
		"blank base URL": {
			env:      map[string]string{config.EnvBaseURL: "   "},
			variable: config.EnvBaseURL,
		},
		"relative base URL": {
			env:      map[string]string{config.EnvBaseURL: "pulse.example.com"},
			variable: config.EnvBaseURL,
		},
		"unsupported base URL scheme": {
			env:      map[string]string{config.EnvBaseURL: "ftp://pulse.example.com"},
			variable: config.EnvBaseURL,
		},
		"base URL with credentials": {
			env:      map[string]string{config.EnvBaseURL: "https://admin:hunter2@pulse.example.com"},
			variable: config.EnvBaseURL,
		},
		"base URL with query": {
			env:      map[string]string{config.EnvBaseURL: "https://pulse.example.com?token=abc"},
			variable: config.EnvBaseURL,
		},
		"base URL with fragment": {
			env:      map[string]string{config.EnvBaseURL: "https://pulse.example.com#top"},
			variable: config.EnvBaseURL,
		},
		"addr without port": {
			env:      merge(minimal(), map[string]string{config.EnvAddr: "127.0.0.1"}),
			variable: config.EnvAddr,
		},
		"addr with non-numeric port": {
			env:      merge(minimal(), map[string]string{config.EnvAddr: "127.0.0.1:http"}),
			variable: config.EnvAddr,
		},
		"addr with out of range port": {
			env:      merge(minimal(), map[string]string{config.EnvAddr: ":70000"}),
			variable: config.EnvAddr,
		},
		"unknown log level": {
			env:      merge(minimal(), map[string]string{config.EnvLogLevel: "verbose"}),
			variable: config.EnvLogLevel,
		},
		"unknown log format": {
			env:      merge(minimal(), map[string]string{config.EnvLogFormat: "logfmt"}),
			variable: config.EnvLogFormat,
		},
		"unparsable shutdown timeout": {
			env:      merge(minimal(), map[string]string{config.EnvShutdownTimeout: "soon"}),
			variable: config.EnvShutdownTimeout,
		},
		"negative shutdown timeout": {
			env:      merge(minimal(), map[string]string{config.EnvShutdownTimeout: "-1s"}),
			variable: config.EnvShutdownTimeout,
		},
		"excessive shutdown timeout": {
			env:      merge(minimal(), map[string]string{config.EnvShutdownTimeout: "10m"}),
			variable: config.EnvShutdownTimeout,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Load(config.Map(tc.env))
			if err == nil {
				t.Fatalf("Load() error = nil, want a validation error")
			}
			if cfg != (config.Config{}) {
				t.Errorf("Load() returned a populated config alongside an error: %+v", cfg)
			}

			var validation *config.ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("Load() error type = %T, want *config.ValidationError", err)
			}
			if got := validation.Variables(); len(got) != 1 || got[0] != tc.variable {
				t.Errorf("failing variables = %v, want [%s]", got, tc.variable)
			}
		})
	}
}

// TestValidationErrorDoesNotLeakValues is the guard behind the acceptance
// criterion that startup diagnostics never expose a configured value. Any
// future variable that carries a secret inherits this behaviour only if
// validation keeps reporting the variable name rather than its content.
func TestValidationErrorDoesNotLeakValues(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-value-that-must-never-be-logged"

	_, err := config.Load(config.Map(map[string]string{
		config.EnvBaseURL:         "https://pulse.example.com/?token=" + secret,
		config.EnvLogLevel:        secret,
		config.EnvLogFormat:       secret,
		config.EnvShutdownTimeout: secret,
		config.EnvAddr:            secret,
	}))
	if err == nil {
		t.Fatal("Load() error = nil, want a validation error")
	}

	message := err.Error()
	if strings.Contains(message, secret) {
		t.Fatalf("error message leaked a configured value: %s", message)
	}
	for _, variable := range []string{
		config.EnvAddr, config.EnvBaseURL, config.EnvLogLevel,
		config.EnvLogFormat, config.EnvShutdownTimeout,
	} {
		if !strings.Contains(message, variable) {
			t.Errorf("error message does not name %s: %s", variable, message)
		}
	}
	if strings.Contains(message, "\n") {
		t.Errorf("error message should stay on one line, got: %q", message)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	_, err := config.Load(config.Map(map[string]string{
		config.EnvLogLevel:  "verbose",
		config.EnvLogFormat: "logfmt",
	}))

	var validation *config.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Load() error type = %T, want *config.ValidationError", err)
	}
	if got, want := len(validation.Problems), 3; got != want {
		t.Fatalf("problems = %d (%v), want %d", got, validation.Variables(), want)
	}
}

func TestProbeURL(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		addr string
		want string
	}{
		"default wildcard": {addr: "", want: "http://127.0.0.1:8080/readyz"},
		"explicit wildcard": {addr: "0.0.0.0:8080",
			want: "http://127.0.0.1:8080/readyz"},
		"ipv6 wildcard": {addr: "[::]:9000", want: "http://127.0.0.1:9000/readyz"},
		"explicit host": {addr: "127.0.0.1:9000", want: "http://127.0.0.1:9000/readyz"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env := map[string]string{}
			if tc.addr != "" {
				env[config.EnvAddr] = tc.addr
			}
			got, err := config.ProbeURL(config.Map(env), "/readyz")
			if err != nil {
				t.Fatalf("ProbeURL() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("ProbeURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProbeURLRejectsInvalidAddr(t *testing.T) {
	t.Parallel()

	if _, err := config.ProbeURL(config.Map(map[string]string{config.EnvAddr: "nonsense"}), "/readyz"); err == nil {
		t.Fatal("ProbeURL() error = nil, want a validation error")
	}
}

func merge(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
