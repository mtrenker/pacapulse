// Package config loads and validates the PacaPulse runtime configuration.
//
// Configuration is read from the process environment only. Validation is
// fail-fast: Load reports every problem it finds and the service refuses to
// start. Diagnostics name the variable and the expectation but never echo the
// configured value, so a mistyped secret cannot leak through startup output.
package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment variables recognised by the service.
const (
	EnvAddr            = "PACAPULSE_ADDR"
	EnvBaseURL         = "PACAPULSE_BASE_URL"
	EnvDataDir         = "PACAPULSE_DATA_DIR"
	EnvLogLevel        = "PACAPULSE_LOG_LEVEL"
	EnvLogFormat       = "PACAPULSE_LOG_FORMAT"
	EnvShutdownTimeout = "PACAPULSE_SHUTDOWN_TIMEOUT"
	// #nosec G101 -- this is an environment variable name, not a credential.
	EnvVisitorSecret     = "PACAPULSE_VISITOR_SECRET"
	EnvTrustedProxyCIDRs = "PACAPULSE_TRUSTED_PROXY_CIDRS"
)

// Defaults applied when an optional variable is unset or empty.
const (
	DefaultAddr            = ":8080"
	DefaultDataDir         = "./data"
	DefaultLogLevel        = "info"
	DefaultLogFormat       = FormatJSON
	DefaultShutdownTimeout = 15 * time.Second
)

// Supported log output formats.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// HTTP server bounds. These are deliberately fixed rather than configurable:
// they exist to cap the resources a single connection can hold, and no
// deployment has yet needed to tune them. Later issues may promote one to an
// environment variable when a concrete workload requires it.
const (
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 15 * time.Second
	WriteTimeout      = 30 * time.Second
	IdleTimeout       = 60 * time.Second
	MaxHeaderBytes    = 64 << 10
)

// MaxShutdownTimeout bounds the configurable termination grace period. It stays
// below the Kubernetes default `terminationGracePeriodSeconds` of 30s so that a
// misconfiguration cannot turn a rollout into a forced kill.
const MaxShutdownTimeout = 25 * time.Second

// Config is the validated runtime configuration of the service.
type Config struct {
	// Addr is the TCP listen address in host:port form. An empty host listens
	// on all interfaces; port 0 selects an ephemeral port.
	Addr string
	// BaseURL is the public origin the service is reachable at, normalised
	// without a trailing slash. Later issues use it to build beacon snippets
	// and OIDC redirect URIs.
	BaseURL string
	// DataDir is the directory the service owns its SQLite database and
	// advisory lockfile in. Only the directory is configurable: the file names
	// beneath it belong to the application, and no SQLite DSN is accepted from
	// the environment.
	DataDir string
	// LogLevel is the minimum level emitted by the application logger.
	LogLevel slog.Level
	// LogFormat selects the structured log encoding.
	LogFormat string
	// ShutdownTimeout bounds in-flight request completion after a termination
	// signal.
	ShutdownTimeout time.Duration
	// VisitorSecret is the decoded master secret used only for transient
	// source limiting and daily visitor derivation.
	VisitorSecret []byte
	// TrustedProxyCIDRs identifies immediate peers whose X-Forwarded-For chain
	// may be considered. An empty slice means no proxy is trusted.
	TrustedProxyCIDRs []netip.Prefix

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

// Lookup resolves an environment variable, reporting whether it was set. It
// mirrors os.LookupEnv so tests can supply a fixture without mutating the
// process environment.
type Lookup func(key string) (string, bool)

// Map returns a Lookup backed by the given values.
func Map(values map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

// Problem records a single configuration variable that failed validation. It
// intentionally has no field for the offending value.
type Problem struct {
	// Variable is the environment variable at fault.
	Variable string
	// Requirement describes what the variable must contain.
	Requirement string
}

func (p Problem) String() string { return p.Variable + " " + p.Requirement }

// ValidationError reports every configuration problem found in one pass, so an
// operator can fix a broken deployment without a restart per mistake.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.String())
	}
	return "invalid configuration: " + strings.Join(parts, "; ")
}

// Variables returns the names of the variables that failed validation.
func (e *ValidationError) Variables() []string {
	names := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		names = append(names, p.Variable)
	}
	return names
}

// Load reads and validates the configuration. On failure it returns a
// *ValidationError describing every problem found.
func Load(lookup Lookup) (Config, error) {
	cfg := Config{
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
	}
	var problems []Problem
	fail := func(variable, requirement string) {
		problems = append(problems, Problem{Variable: variable, Requirement: requirement})
	}

	addr := value(lookup, EnvAddr, DefaultAddr)
	if err := validateAddr(addr); err != nil {
		fail(EnvAddr, err.Error())
	} else {
		cfg.Addr = addr
	}

	baseURL, ok := lookup(EnvBaseURL)
	baseURL = strings.TrimSpace(baseURL)
	switch {
	case !ok || baseURL == "":
		fail(EnvBaseURL, "is required and must be an absolute http or https URL, for example https://pulse.example.com")
	default:
		normalised, err := normaliseBaseURL(baseURL)
		if err != nil {
			fail(EnvBaseURL, err.Error())
		} else {
			cfg.BaseURL = normalised
		}
	}

	dataDir := value(lookup, EnvDataDir, DefaultDataDir)
	if err := validateDataDir(dataDir); err != nil {
		fail(EnvDataDir, err.Error())
	} else {
		cfg.DataDir = filepath.Clean(dataDir)
	}

	level := strings.ToLower(value(lookup, EnvLogLevel, DefaultLogLevel))
	if parsed, err := parseLevel(level); err != nil {
		fail(EnvLogLevel, err.Error())
	} else {
		cfg.LogLevel = parsed
	}

	format := strings.ToLower(value(lookup, EnvLogFormat, DefaultLogFormat))
	switch format {
	case FormatJSON, FormatText:
		cfg.LogFormat = format
	default:
		fail(EnvLogFormat, "must be one of json, text")
	}

	timeout := value(lookup, EnvShutdownTimeout, DefaultShutdownTimeout.String())
	parsed, err := time.ParseDuration(timeout)
	switch {
	case err != nil:
		fail(EnvShutdownTimeout, "must be a Go duration, for example 15s")
	case parsed <= 0 || parsed > MaxShutdownTimeout:
		fail(EnvShutdownTimeout, "must be a positive duration of at most "+MaxShutdownTimeout.String())
	default:
		cfg.ShutdownTimeout = parsed
	}

	secret, ok := lookup(EnvVisitorSecret)
	decoded, secretErr := decodeVisitorSecret(secret)
	switch {
	case !ok || strings.TrimSpace(secret) == "":
		fail(EnvVisitorSecret, "is required and must be strict standard base64 decoding to at least 32 bytes")
	case secretErr != nil:
		fail(EnvVisitorSecret, "must be strict standard base64 decoding to at least 32 bytes")
	default:
		cfg.VisitorSecret = decoded
	}

	trusted, err := parseTrustedProxyCIDRs(lookup)
	if err != nil {
		fail(EnvTrustedProxyCIDRs, "must be a comma-separated list of valid CIDR prefixes, or blank to trust no proxies")
	} else {
		cfg.TrustedProxyCIDRs = trusted
	}

	if len(problems) > 0 {
		return Config{}, &ValidationError{Problems: problems}
	}
	return cfg, nil
}

// decodeVisitorSecret enforces canonical padded RFC 4648 standard base64 and
// the minimum entropy-bearing byte length without retaining the encoded form.
func decodeVisitorSecret(raw string) ([]byte, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || strings.ContainsAny(raw, "\r\n\t ") {
		return nil, fmt.Errorf("invalid encoding")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(raw)
	if err != nil || len(decoded) < 32 || base64.StdEncoding.EncodeToString(decoded) != raw {
		return nil, fmt.Errorf("invalid encoding")
	}
	return decoded, nil
}

func parseTrustedProxyCIDRs(lookup Lookup) ([]netip.Prefix, error) {
	raw, ok := lookup(EnvTrustedProxyCIDRs)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// value returns the trimmed variable, falling back to fallback when the
// variable is unset or blank.
func value(lookup Lookup, key, fallback string) string {
	raw, ok := lookup(key)
	if !ok {
		return fallback
	}
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		return trimmed
	}
	return fallback
}

func validateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be a host:port listen address, for example :8080")
	}
	if strings.ContainsAny(host, " \t") {
		return fmt.Errorf("must be a host:port listen address, for example :8080")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return fmt.Errorf("must use a numeric port between 0 and 65535")
	}
	return nil
}

// validateDataDir rejects a path the service could never own its database in.
// Whether the directory can actually be created, written to, and locked is
// decided by storage at startup, where the answer is authoritative; repeating
// the filesystem check here would only add a race.
func validateDataDir(dir string) error {
	if strings.ContainsRune(dir, 0) {
		return fmt.Errorf("must be a filesystem path, for example /var/lib/pacapulse")
	}
	if filepath.Clean(dir) == string(filepath.Separator) {
		return fmt.Errorf("must be a directory the service can own, not the filesystem root")
	}
	return nil
}

// normaliseBaseURL validates the public origin and strips any trailing slash so
// callers can concatenate paths without producing double separators.
func normaliseBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("must be an absolute http or https URL, for example https://pulse.example.com")
	}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return "", fmt.Errorf("must use the http or https scheme")
	case parsed.Host == "":
		return "", fmt.Errorf("must include a host, for example https://pulse.example.com")
	case parsed.User != nil:
		return "", fmt.Errorf("must not embed credentials")
	case parsed.RawQuery != "" || parsed.ForceQuery:
		return "", fmt.Errorf("must not include a query string")
	case parsed.Fragment != "":
		return "", fmt.Errorf("must not include a fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func parseLevel(name string) (slog.Level, error) {
	switch name {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("must be one of debug, info, warn, error")
	}
}

// ProbeURL derives the loopback URL used by the container health check from the
// listen address alone. The container health check must work before, and
// independently of, the rest of the configuration, so it deliberately does not
// call Load.
func ProbeURL(lookup Lookup, path string) (string, error) {
	addr := value(lookup, EnvAddr, DefaultAddr)
	if err := validateAddr(addr); err != nil {
		return "", &ValidationError{Problems: []Problem{{Variable: EnvAddr, Requirement: err.Error()}}}
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", &ValidationError{Problems: []Problem{{Variable: EnvAddr, Requirement: "must be a host:port listen address, for example :8080"}}}
	}
	// A wildcard listener is not routable as a destination; probe loopback.
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
		Path:   path,
	}).String(), nil
}
