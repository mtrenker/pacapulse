package collector

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestClientAddressTrustBoundary(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:ffff::/48")}
	tests := map[string]struct {
		peer string
		xff  string
		want string
		ok   bool
	}{
		"direct peer":             {"192.0.2.8:123", "", "192.0.2.8", true},
		"untrusted spoof ignored": {"192.0.2.8:123", "203.0.113.9", "192.0.2.8", true},
		"trusted chain":           {"10.0.0.2:123", "203.0.113.9, 10.1.2.3", "203.0.113.9", true},
		"mapped address unmapped": {"[::ffff:192.0.2.9]:123", "", "192.0.2.9", true},
		"malformed trusted chain": {"10.0.0.2:123", "203.0.113.9, nope", "", false},
		"only trusted proxies":    {"10.0.0.2:123", "10.1.2.3", "", false},
		"missing proxy chain":     {"10.0.0.2:123", "", "", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = tc.peer
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			got, ok := clientAddress(req, trusted)
			if ok != tc.ok {
				t.Fatalf("available = %v, want %v", ok, tc.ok)
			}
			if ok && got.String() != tc.want {
				t.Errorf("address = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUserAgentClassificationPrecedence(t *testing.T) {
	tests := map[string]struct{ ua, browser, device string }{
		"edge with chrome": {"Mozilla Windows NT Chrome/120 Edg/120", "edge", "desktop"},
		"crios iphone":     {"Mozilla iPhone Mobile CriOS/120 Safari/604", "chromium", "mobile"},
		"safari ipad":      {"Mozilla iPad Version/17 Safari/605", "safari", "tablet"},
		"android tablet":   {"Mozilla Linux Android Chrome/120 Safari/537", "chromium", "tablet"},
		"firefox desktop":  {"Mozilla X11 Firefox/122", "firefox", "desktop"},
		"unknown":          {"curl", "other", "other"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			browser, device := classifyUserAgent(tc.ua)
			if browser != tc.browser || device != tc.device {
				t.Errorf("class = %s/%s, want %s/%s", browser, device, tc.browser, tc.device)
			}
		})
	}
}

func TestVisitorKeyExactAndSeparated(t *testing.T) {
	at := time.Date(2026, 3, 14, 23, 59, 59, 0, time.UTC)
	address := netip.MustParseAddr("192.0.2.8")
	key := visitorKey([]byte(fixtureSecret), "11111111-1111-4111-8111-111111111111", at, address, "chromium", "desktop")
	if got, want := hex.EncodeToString(key[:]), "1de28a0ad1015907178901e8d9cbe696dc63cbcebd46bdcd86a5b30fe1b92f11"; got != want {
		t.Errorf("visitor key = %s, want %s", got, want)
	}
	variants := [][32]byte{
		visitorKey([]byte(fixtureSecret), "22222222-2222-4222-8222-222222222222", at, address, "chromium", "desktop"),
		visitorKey([]byte(fixtureSecret), "11111111-1111-4111-8111-111111111111", at.Add(24*time.Hour), address, "chromium", "desktop"),
		visitorKey([]byte(fixtureSecret), "11111111-1111-4111-8111-111111111111", at, address, "firefox", "desktop"),
		visitorKey([]byte(fixtureSecret), "11111111-1111-4111-8111-111111111111", at, address, "chromium", "mobile"),
	}
	for index, variant := range variants {
		if variant == key {
			t.Errorf("variant %d did not separate the visitor key", index)
		}
	}
}
