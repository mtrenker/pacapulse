package collector

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/text/unicode/norm"
)

const (
	maxBodyBytes     = 4096
	maxPathBytes     = 512
	maxHostBytes     = 253
	maxCampaignBytes = 100
)

type payload struct {
	Version      int
	Site         string
	EventID      string
	Path         string
	ReferrerHost string
	UTMSource    string
	UTMMedium    string
	UTMCampaign  string
	DNT          bool
}

func parsePayload(body []byte) (payload, error) {
	if !utf8.Valid(body) {
		return payload{}, fmt.Errorf("invalid payload")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return payload{}, fmt.Errorf("invalid payload")
	}
	members := make(map[string]json.RawMessage, 9)
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return payload{}, fmt.Errorf("invalid payload")
		}
		name, ok := nameToken.(string)
		if !ok {
			return payload{}, fmt.Errorf("invalid payload")
		}
		if _, duplicate := members[name]; duplicate {
			return payload{}, fmt.Errorf("invalid payload")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return payload{}, fmt.Errorf("invalid payload")
		}
		members[name] = raw
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return payload{}, fmt.Errorf("invalid payload")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return payload{}, fmt.Errorf("invalid payload")
	}

	allowed := map[string]bool{
		"v": true, "site": true, "event_id": true, "path": true,
		"referrer_host": true, "utm_source": true, "utm_medium": true,
		"utm_campaign": true, "dnt": true,
	}
	for name := range members {
		if !allowed[name] {
			return payload{}, fmt.Errorf("invalid payload")
		}
	}
	for _, required := range []string{"v", "site", "event_id", "path"} {
		if _, ok := members[required]; !ok {
			return payload{}, fmt.Errorf("invalid payload")
		}
	}

	var out payload
	var version json.Number
	if err := json.Unmarshal(members["v"], &version); err != nil || version.String() != "1" {
		return payload{}, fmt.Errorf("invalid payload")
	}
	out.Version = 1
	for name, target := range map[string]*string{
		"site": &out.Site, "event_id": &out.EventID, "path": &out.Path,
	} {
		if err := json.Unmarshal(members[name], target); err != nil {
			return payload{}, fmt.Errorf("invalid payload")
		}
	}
	for name, target := range map[string]*string{
		"referrer_host": &out.ReferrerHost, "utm_source": &out.UTMSource,
		"utm_medium": &out.UTMMedium, "utm_campaign": &out.UTMCampaign,
	} {
		if raw, ok := members[name]; ok {
			if err := json.Unmarshal(raw, target); err != nil {
				return payload{}, fmt.Errorf("invalid payload")
			}
		}
	}
	if raw, ok := members["dnt"]; ok {
		if err := json.Unmarshal(raw, &out.DNT); err != nil || !out.DNT {
			return payload{}, fmt.Errorf("invalid payload")
		}
	}
	if !validCollectionKey(out.Site) || !isCanonicalUUIDv4(out.EventID) {
		return payload{}, fmt.Errorf("invalid payload")
	}
	canonicalPath, ok := canonicalizePath(out.Path)
	if !ok {
		return payload{}, fmt.Errorf("invalid payload")
	}
	out.Path = canonicalPath
	return out, nil
}

func validCollectionKey(value string) bool {
	if len(value) != 35 || !strings.HasPrefix(value, "pp_") {
		return false
	}
	for i := 3; i < len(value); i++ {
		c := value[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func isCanonicalUUIDv4(value string) bool {
	if len(value) != 36 || value[14] != '4' {
		return false
	}
	if !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	for i, c := range []byte(value) {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

func canonicalizePath(value string) (string, bool) {
	if value == "" || len(value) > maxPathBytes || !utf8.ValidString(value) || value[0] != '/' || strings.ContainsAny(value, "?#") {
		return "", false
	}
	bytes := []byte(value)
	for i := 0; i < len(bytes); i++ {
		if bytes[i] != '%' {
			continue
		}
		if i+2 >= len(bytes) || !isHex(bytes[i+1]) || !isHex(bytes[i+2]) {
			return "", false
		}
		bytes[i+1] = upperHex(bytes[i+1])
		bytes[i+2] = upperHex(bytes[i+2])
		i += 2
	}
	return string(bytes), true
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
func upperHex(c byte) byte {
	if c >= 'a' && c <= 'f' {
		return c - ('a' - 'A')
	}
	return c
}

func normalizeReferrerHost(value string) (string, bool) {
	if len(value) > maxHostBytes {
		return "", false
	}
	if host, port, err := net.SplitHostPort(value); err == nil {
		number, numberErr := strconv.Atoi(port)
		if numberErr != nil || number < 0 || number > 65535 {
			return "", false
		}
		value = host
	}
	return normalizeHost(value, false)
}

func normalizeHost(value string, allowIP bool) (string, bool) {
	if value == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return "", false
	}
	value = strings.TrimSuffix(value, ".")
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = value[1 : len(value)-1]
	}
	if addr, err := netip.ParseAddr(value); err == nil {
		if !allowIP || addr.Zone() != "" {
			return "", false
		}
		return addr.Unmap().String(), true
	}
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil {
		return "", false
	}
	ascii = strings.ToLower(strings.TrimSuffix(ascii, "."))
	if ascii == "" || len(ascii) > maxHostBytes || strings.ContainsAny(ascii, "/:@[]") {
		return "", false
	}
	return ascii, true
}

func normalizeCampaign(value string) string {
	value = norm.NFC.String(value)
	value = strings.TrimFunc(value, unicode.IsSpace)
	value = strings.ToLower(value)
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	if value == "" || len(value) > maxCampaignBytes || !utf8.ValidString(value) {
		return ""
	}
	return value
}

func acceptedOrigin(values []string, hosts map[string]struct{}, httpLoopbackSite bool) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	raw := values[0]
	if raw == "" || raw == "null" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, ", \t\r\n?#") {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || !validOriginPort(parsed.Host) || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", false
	}
	host, ok := normalizeHost(parsed.Hostname(), true)
	if !ok {
		return "", false
	}
	if _, ok := hosts[host]; !ok {
		return "", false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		addr, addressErr := netip.ParseAddr(host)
		if !httpLoopbackSite || host != "localhost" && (addressErr != nil || !addr.IsLoopback()) {
			return "", false
		}
	default:
		return "", false
	}
	return raw, true
}

func validOriginPort(host string) bool {
	remainder := ""
	if strings.HasPrefix(host, "[") {
		closing := strings.LastIndexByte(host, ']')
		if closing < 0 {
			return false
		}
		remainder = host[closing+1:]
	} else if colon := strings.LastIndexByte(host, ':'); colon >= 0 {
		remainder = host[colon:]
	}
	if remainder == "" {
		return true
	}
	if !strings.HasPrefix(remainder, ":") || len(remainder) == 1 {
		return false
	}
	port, err := strconv.Atoi(remainder[1:])
	return err == nil && port >= 0 && port <= 65535
}
