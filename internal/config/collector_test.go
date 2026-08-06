package config_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/mtrenker/pacapulse/internal/config"
)

func TestCollectorConfiguration(t *testing.T) {
	env := minimal()
	env[config.EnvTrustedProxyCIDRs] = "10.0.0.1/8, 2001:db8::/32"
	cfg, err := config.Load(config.Map(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.VisitorSecret) != 32 {
		t.Errorf("secret bytes = %d, want 32", len(cfg.VisitorSecret))
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8::/32")}
	if len(cfg.TrustedProxyCIDRs) != len(want) {
		t.Fatalf("prefixes = %v", cfg.TrustedProxyCIDRs)
	}
	for index := range want {
		if cfg.TrustedProxyCIDRs[index] != want[index] {
			t.Errorf("prefix %d = %v, want %v", index, cfg.TrustedProxyCIDRs[index], want[index])
		}
	}
}

func TestCollectorConfigurationRejectsSecretsAndCIDRsWithoutLeaking(t *testing.T) {
	const bad = "sensitive-invalid-value"
	tests := []map[string]string{
		{config.EnvBaseURL: "https://pulse.example.com"},
		{config.EnvBaseURL: "https://pulse.example.com", config.EnvVisitorSecret: "c2hvcnQ="},
		{config.EnvBaseURL: "https://pulse.example.com", config.EnvVisitorSecret: testVisitorSecret + "\n"},
		{config.EnvBaseURL: "https://pulse.example.com", config.EnvVisitorSecret: testVisitorSecret, config.EnvTrustedProxyCIDRs: bad},
	}
	for _, env := range tests {
		_, err := config.Load(config.Map(env))
		var validation *config.ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("error = %v, want validation error", err)
		}
		if strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), "c2hvcnQ=") || strings.Contains(err.Error(), testVisitorSecret) {
			t.Errorf("error leaked configured value: %v", err)
		}
	}
}
