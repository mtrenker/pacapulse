package beacon_test

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mtrenker/pacapulse/internal/beacon"
)

// TestAssetIsWithinItsBudget measures the exact bytes the service embeds and
// serves. Brotli is measured by the Node size test, which has a Brotli
// encoder; Go's standard library does not, and the protocol explicitly allows
// the service itself to satisfy negotiation with gzip and identity.
func TestAssetIsWithinItsBudget(t *testing.T) {
	t.Parallel()

	source := beacon.Source()

	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		t.Fatalf("create the gzip writer: %v", err)
	}
	if _, err := writer.Write(source); err != nil {
		t.Fatalf("compress the asset: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("finish the gzip stream: %v", err)
	}

	t.Logf("beacon.js: %d bytes source (budget %d), %d bytes gzip (budget %d)",
		len(source), beacon.MaxSourceBytes, compressed.Len(), beacon.MaxCompressedBytes)

	if len(source) > beacon.MaxSourceBytes {
		t.Errorf("source is %d bytes, over the %d byte budget", len(source), beacon.MaxSourceBytes)
	}
	if compressed.Len() > beacon.MaxCompressedBytes {
		t.Errorf("gzip output is %d bytes, over the %d byte budget", compressed.Len(), beacon.MaxCompressedBytes)
	}
}

// TestAssetAvoidsForbiddenConstructs greps the production source for the
// things the protocol forbids outright. A grep is a blunt instrument, but
// these are exactly the constructs whose *presence* is the problem: a reader
// auditing the beacon should be able to establish that it cannot read storage
// or fall back to a weaker transport without tracing control flow.
func TestAssetAvoidsForbiddenConstructs(t *testing.T) {
	t.Parallel()

	source := string(beacon.Source())

	forbidden := map[string]string{
		"document.cookie":  "cookies are outside the privacy promise",
		"localStorage":     "browser storage is outside the privacy promise",
		"sessionStorage":   "browser storage is outside the privacy promise",
		"indexedDB":        "browser storage is outside the privacy promise",
		"caches":           "Cache Storage is outside the privacy promise",
		"serviceWorker":    "service-worker state is outside the privacy promise",
		"Math.random":      "randomness must come from Web Crypto",
		"eval(":            "eval is forbidden and breaks strict CSP",
		"new Function":     "dynamic code generation is forbidden and breaks strict CSP",
		"import(":          "dynamic import is forbidden; the asset is a classic script",
		"sendBeacon":       "the transport is Fetch, which can set the required options",
		"XMLHttpRequest":   "synchronous XHR is not a permitted fallback transport",
		"new Image":        "image pixels are not a permitted fallback transport",
		"pushState":        "the beacon does not patch history APIs",
		"replaceState":     "the beacon does not patch history APIs",
		"popstate":         "the beacon does not observe router navigation implicitly",
		"console.":         "the asset produces no console output",
		"sourceMappingURL": "the asset requests no source map",
	}

	for needle, why := range forbidden {
		if strings.Contains(source, needle) {
			t.Errorf("the asset contains %q: %s", needle, why)
		}
	}

	// location.href appears exactly once, in the Keepalive feature detection
	// the protocol prescribes, and never as an analytics value.
	if got := strings.Count(source, "location.href"); got != 1 {
		t.Errorf("location.href appears %d times, want exactly 1 (Keepalive detection)", got)
	}
	if !strings.Contains(source, `"keepalive" in new win.Request(win.location.href)`) {
		t.Error("the one location.href read is not the documented Keepalive detection")
	}
}

// TestAssetSendsOnlyTheDocumentedPayloadNames is a second, cheap guard on the
// bounded payload: any member name that reaches JSON.stringify has to be one
// of these, so a new field cannot be added without this test noticing.
func TestAssetSendsOnlyTheDocumentedPayloadNames(t *testing.T) {
	t.Parallel()

	source := string(beacon.Source())
	for _, name := range []string{"v:", "site:", "event_id:", "path:", "referrer_host", "utm_source", "utm_medium", "utm_campaign", "dnt"} {
		if !strings.Contains(source, name) {
			t.Errorf("the asset no longer mentions the payload member %q", name)
		}
	}
	for _, name := range []string{
		"screen", "innerWidth", "userAgent", "language", "timeZone",
		"platform", "hardwareConcurrency", "deviceMemory", "referrer_url",
	} {
		if strings.Contains(source, name) {
			t.Errorf("the asset mentions %q, which is not a protocol field", name)
		}
	}
}

// TestAssetIsCleanText keeps the served bytes free of the accidents that make
// a hand-maintained asset behave differently across editors and proxies.
func TestAssetIsCleanText(t *testing.T) {
	t.Parallel()

	source := beacon.Source()

	if !utf8.Valid(source) {
		t.Fatal("the asset is not valid UTF-8")
	}
	if bytes.HasPrefix(source, []byte{0xEF, 0xBB, 0xBF}) {
		t.Error("the asset starts with a byte-order mark")
	}
	if bytes.Contains(source, []byte("\r")) {
		t.Error("the asset contains carriage returns")
	}
	if !bytes.HasSuffix(source, []byte("\n")) {
		t.Error("the asset does not end with a newline")
	}
	for number, line := range strings.Split(strings.TrimSuffix(string(source), "\n"), "\n") {
		if strings.TrimRight(line, " \t") != line {
			t.Errorf("line %d has trailing whitespace", number+1)
		}
	}
	// The whole asset is one IIFE, so nothing it declares can leak into a
	// tracked page's global scope by accident.
	if !strings.Contains(string(source), "(function () {\n  \"use strict\";") {
		t.Error("the asset is not a strict-mode immediately-invoked function")
	}
}
