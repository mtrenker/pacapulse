package collector

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

func encode(parts ...[]byte) []byte {
	size := 4 * len(parts)
	for _, part := range parts {
		size += len(part)
	}
	out := make([]byte, 0, size)
	var length [4]byte
	for _, part := range parts {
		// #nosec G115 -- protocol components are bounded to at most 512 bytes.
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		out = append(out, length[:]...)
		out = append(out, part...)
	}
	return out
}

func keyedHMAC(secret, message []byte) [32]byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(message)
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

func visitorKey(secret []byte, siteID string, at time.Time, address netip.Addr, browser, device string) [32]byte {
	day := at.UTC().Format("2006-01-02")
	dayKey := keyedHMAC(secret, encode(
		[]byte("pacapulse/visitor-day/v1"), []byte(siteID), []byte(day),
	))
	return keyedHMAC(dayKey[:], encode(addressBytes(address), []byte(browser), []byte(device)))
}

func sourceKey(secret []byte, siteID string, at time.Time, address netip.Addr) [32]byte {
	return keyedHMAC(secret, encode(
		[]byte("pacapulse/source-limit/v1"), []byte(siteID),
		[]byte(at.UTC().Format("2006-01-02")), addressBytes(address),
	))
}

func addressBytes(address netip.Addr) []byte {
	address = address.Unmap()
	if address.Is4() {
		value := address.As4()
		return value[:]
	}
	value := address.As16()
	return value[:]
}

func clientAddress(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || peer.Zone() != "" {
		return netip.Addr{}, false
	}
	peer = peer.Unmap()
	if !inPrefixes(peer, trusted) {
		return peer, true
	}

	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return netip.Addr{}, false
	}
	var chain []netip.Addr
	for _, value := range values {
		for _, member := range strings.Split(value, ",") {
			member = strings.TrimSpace(member)
			address, err := netip.ParseAddr(member)
			if err != nil || address.Zone() != "" {
				return netip.Addr{}, false
			}
			chain = append(chain, address.Unmap())
		}
	}
	for index := len(chain) - 1; index >= 0; index-- {
		if !inPrefixes(chain[index], trusted) {
			return chain[index], true
		}
	}
	return netip.Addr{}, false
}

func inPrefixes(address netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func classifyUserAgent(userAgent string) (browser, device string) {
	ua := strings.ToLower(userAgent)
	switch {
	case containsAny(ua, "edg/", "edgios/", "edga/"):
		browser = "edge"
	case containsAny(ua, "chromium/", "chrome/", "crios/"):
		browser = "chromium"
	case containsAny(ua, "firefox/", "fxios/"):
		browser = "firefox"
	case strings.Contains(ua, "safari/") && (strings.Contains(ua, "version/") || containsAny(ua, "iphone", "ipad", "ipod")):
		browser = "safari"
	default:
		browser = "other"
	}

	switch {
	case containsAny(ua, "ipad", "tablet") || strings.Contains(ua, "android") && !strings.Contains(ua, "mobile"):
		device = "tablet"
	case containsAny(ua, "mobile", "iphone", "ipod"):
		device = "mobile"
	case containsAny(ua, "windows nt", "macintosh", "x11", "linux"):
		device = "desktop"
	default:
		device = "other"
	}
	return browser, device
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
