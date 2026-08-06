package collector

import (
	"encoding/json"
	"testing"
)

func payloadJSON(t *testing.T, overrides map[string]string) []byte {
	t.Helper()
	members := map[string]json.RawMessage{
		"v":        json.RawMessage("1"),
		"site":     json.RawMessage(`"pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ"`),
		"event_id": json.RawMessage(`"550e8400-e29b-41d4-a716-446655440000"`),
		"path":     json.RawMessage(`"/"`),
	}
	for name, raw := range overrides {
		members[name] = json.RawMessage(raw)
	}
	body, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

func TestPayloadVersionRequiresExactIntegerTokenOne(t *testing.T) {
	if _, err := parsePayload(payloadJSON(t, nil)); err != nil {
		t.Fatalf("integer token 1 rejected: %v", err)
	}
	for name, raw := range map[string]string{
		"quoted numeric string": `"1"`,
		"decimal":               "1.0",
		"exponent":              "1e0",
		"other integer":         "2",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePayload(payloadJSON(t, map[string]string{"v": raw})); err == nil {
				t.Fatalf("version %s was accepted", raw)
			}
		})
	}
}

func TestPayloadStringMembersRejectNullAndOtherWrongTypes(t *testing.T) {
	for _, member := range []string{
		"site", "event_id", "path", "referrer_host",
		"utm_source", "utm_medium", "utm_campaign",
	} {
		for name, raw := range map[string]string{"null": "null", "object": `{}`} {
			t.Run(member+"/"+name, func(t *testing.T) {
				if _, err := parsePayload(payloadJSON(t, map[string]string{member: raw})); err == nil {
					t.Fatalf("%s %s was accepted", member, raw)
				}
			})
		}
	}
}

func TestPayloadAllowsAbsentOptionalStringMembers(t *testing.T) {
	payload, err := parsePayload(payloadJSON(t, nil))
	if err != nil {
		t.Fatalf("parsePayload() error = %v", err)
	}
	if payload.ReferrerHost != "" || payload.UTMSource != "" || payload.UTMMedium != "" || payload.UTMCampaign != "" {
		t.Errorf("absent optionals became populated: %+v", payload)
	}
}

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
