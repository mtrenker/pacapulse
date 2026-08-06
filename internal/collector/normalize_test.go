package collector

import "testing"

func TestCanonicalization(t *testing.T) {
	if got, ok := canonicalizePath("/docs/%7euser"); !ok || got != "/docs/%7Euser" {
		t.Errorf("path = %q, %v", got, ok)
	}
	for _, invalid := range []string{"", "relative", "/query?x", "/fragment#x", "/bad/%z0", "/bad/%"} {
		if _, ok := canonicalizePath(invalid); ok {
			t.Errorf("accepted invalid path %q", invalid)
		}
	}
	if got, ok := normalizeHost("BÜCHER.Example.", false); !ok || got != "xn--bcher-kva.example" {
		t.Errorf("host = %q, %v", got, ok)
	}
	if _, ok := normalizeHost("192.0.2.1", false); ok {
		t.Error("accepted IP referrer")
	}
	if got, ok := normalizeReferrerHost("Search.Example:443"); !ok || got != "search.example" {
		t.Errorf("referrer with port = %q, %v", got, ok)
	}
	if got := normalizeCampaign(" \tSPRING\u0301\x00 "); got != "sprinǵ" {
		t.Errorf("campaign = %q", got)
	}
}

func TestMediaTypeParsing(t *testing.T) {
	for _, valid := range []string{"text/plain", "Text/Plain;Charset=UTF-8", "text/plain;charset=\"utf-8\""} {
		if !acceptedMediaType(valid) {
			t.Errorf("rejected %q", valid)
		}
	}
	for _, invalid := range []string{"", "application/json", "text/plain;charset=latin1", "text/plain;foo=bar", "text/plain;charset=utf-8;foo=bar"} {
		if acceptedMediaType(invalid) {
			t.Errorf("accepted %q", invalid)
		}
	}
}

func TestOriginRequiresOneBareAllowedOrigin(t *testing.T) {
	hosts := map[string]struct{}{"shop.example": {}, "localhost": {}, "::1": {}}
	for _, value := range []string{"https://shop.example", "https://SHOP.example:8443", "http://localhost:5173", "http://[::1]:3000"} {
		if got, ok := acceptedOrigin([]string{value}, hosts, true); !ok || got != value {
			t.Errorf("origin %q = %q, %v", value, got, ok)
		}
	}
	for _, values := range [][]string{nil, {"null"}, {"https://shop.example", "https://shop.example"}, {"https://other.example"}, {"http://shop.example"}, {"https://shop.example/"}, {"https://user@shop.example"}, {"https://shop.example?q=x"}, {"https://shop.example#"}, {"https://shop.example:"}, {"https://shop.example:99999"}} {
		if _, ok := acceptedOrigin(values, hosts, false); ok {
			t.Errorf("accepted origins %v", values)
		}
	}
	if _, ok := acceptedOrigin([]string{"http://localhost:5173"}, hosts, false); ok {
		t.Error("production site accepted an HTTP loopback allowed host")
	}
}
