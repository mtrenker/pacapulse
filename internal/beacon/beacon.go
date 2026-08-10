// Package beacon embeds and serves the public browser asset.
//
// beacon.js is the production source and the served representation at the
// same time: it is embedded verbatim, there is no build step, and the only
// transformation the service may apply is a content coding. That is what
// makes the file a reviewable privacy boundary rather than a build artifact —
// what a reader sees in the repository is what a tracked page executes.
//
// The response contract, the cache and security headers, and the size budget
// come from docs/architecture/collection-protocol.md.
package beacon

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Path is the stable major-version asset URL. Compatible correctness,
// compatibility, security, and privacy fixes change this file's content; a
// breaking installation, API, payload, or transport change requires
// /beacon/v2.js instead.
const Path = "/beacon/v1.js"

// Budgets bound the asset. They are protocol limits, not guidelines: a
// beacon that grows without review stops being cheap for every tracked page.
const (
	MaxSourceBytes     = 12 * 1024
	MaxCompressedBytes = 4 * 1024
)

//go:embed beacon.js
var source []byte

// Source returns the exact embedded production asset. The copy exists so a
// caller — in practice a test — cannot mutate what the service serves.
func Source() []byte { return bytes.Clone(source) }

// Handler serves the asset in the identity and gzip content codings.
//
// Both representations and both validators are computed once at construction:
// the asset never changes while the process runs, so per-request compression
// would only spend CPU on a response that caches for an hour anyway.
type Handler struct {
	identity representation
	gzipped  representation
}

type representation struct {
	body []byte
	etag string
}

// NewHandler prepares both representations of the embedded asset.
func NewHandler() *Handler {
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		// BestCompression is a valid level, so this is unreachable; panicking
		// at construction beats serving an empty asset for the process's life.
		panic("beacon: gzip writer: " + err.Error())
	}
	if _, err := writer.Write(source); err != nil {
		panic("beacon: gzip write: " + err.Error())
	}
	if err := writer.Close(); err != nil {
		panic("beacon: gzip close: " + err.Error())
	}

	return &Handler{
		identity: newRepresentation(source),
		gzipped:  newRepresentation(compressed.Bytes()),
	}
}

// newRepresentation pairs a body with a strong validator derived from that
// body.
//
// Each representation is hashed on its own rather than sharing the source's
// digest under distinguishing suffixes. A strong ETag identifies the exact
// bytes sent, and the gzip bytes are not a function of the source alone: a Go
// release that changes compress/flate's output would otherwise serve
// different bytes under an unchanged validator, and every cache holding the
// old copy would keep it. Hashing what is actually sent makes that impossible
// by construction, and it still gives the two representations the distinct
// validators a shared cache needs to tell them apart.
//
// Half the digest is 64 bits of collision resistance over a file that changes
// only when the repository does; a full one only lengthens every response and
// every conditional request.
func newRepresentation(body []byte) representation {
	digest := sha256.Sum256(body)
	return representation{body: body, etag: `"` + hex.EncodeToString(digest[:16]) + `"`}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	// Set before every exit: the response body depends on Accept-Encoding
	// whichever way negotiation goes, including for 304 and 406.
	header.Set("Vary", "Accept-Encoding")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		header.Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	chosen, acceptable := negotiate(r.Header.Values("Accept-Encoding"))
	if !acceptable {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}

	selected := h.identity
	if chosen == codingGzip {
		selected = h.gzipped
		header.Set("Content-Encoding", "gzip")
	}

	header.Set("Content-Type", "application/javascript; charset=utf-8")
	header.Set("Cache-Control", "public, max-age=3600, stale-while-revalidate=86400")
	header.Set("ETag", selected.etag)
	// The asset is loaded cross-origin by every tracked site, so it must be
	// readable across origins while still refusing to be sniffed into another
	// content type.
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Cross-Origin-Resource-Policy", "cross-origin")
	// ServeContent skips Content-Length whenever Content-Encoding is set,
	// because it cannot know that the writer is not compressing on the fly.
	// This one can: the length of the selected representation is exact.
	header.Set("Content-Length", strconv.Itoa(len(selected.body)))

	// ServeContent supplies conditional-request and Range handling against the
	// ETag above. The zero modification time keeps Last-Modified out of the
	// response: the asset's identity is its content, not a build timestamp.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(selected.body))
}

// coding is a content coding this handler can produce.
type coding int

const (
	codingIdentity coding = iota
	codingGzip
)

// negotiate applies RFC 9110 Accept-Encoding rules to the two codings the
// handler has. It reports the chosen coding and whether any coding is
// acceptable at all; when nothing is, the caller owes the client a 406.
//
// Identity is acceptable by default and can only be removed by an explicit
// "identity;q=0" or by a "*;q=0" with no more specific identity entry. gzip
// is the opposite: it is only ever sent when the client asked for it, by name
// or through a positive wildcard.
func negotiate(values []string) (coding, bool) {
	if len(values) == 0 {
		// No Accept-Encoding at all. RFC 9110 permits any coding here, but a
		// client that did not ask is served the plain representation.
		return codingIdentity, true
	}

	var (
		gzipQuality, identityQuality, wildcardQuality float64
		gzipPresent, identityPresent, wildcardPresent bool
	)
	for _, value := range values {
		for _, member := range strings.Split(value, ",") {
			name, quality, ok := parseCoding(member)
			if !ok {
				continue
			}
			switch {
			case strings.EqualFold(name, "gzip"):
				gzipQuality, gzipPresent = quality, true
			case strings.EqualFold(name, "identity"):
				identityQuality, identityPresent = quality, true
			case name == "*":
				wildcardQuality, wildcardPresent = quality, true
			}
		}
	}

	effectiveGzip := 0.0
	switch {
	case gzipPresent:
		effectiveGzip = gzipQuality
	case wildcardPresent:
		effectiveGzip = wildcardQuality
	}
	effectiveIdentity := 1.0
	switch {
	case identityPresent:
		effectiveIdentity = identityQuality
	case wildcardPresent:
		effectiveIdentity = wildcardQuality
	}

	switch {
	// A tie goes to gzip: it is the cheaper response and the client expressed
	// no preference between them.
	case effectiveGzip > 0 && effectiveGzip >= effectiveIdentity:
		return codingGzip, true
	case effectiveIdentity > 0:
		return codingIdentity, true
	default:
		return codingIdentity, false
	}
}

// parseCoding splits one Accept-Encoding member into its coding name and
// quality value. A member whose q parameter is not a valid quality is dropped
// rather than silently treated as q=1.
func parseCoding(member string) (string, float64, bool) {
	name, parameters, _ := strings.Cut(member, ";")
	name = strings.TrimSpace(name)
	if name == "" {
		return "", 0, false
	}

	quality := 1.0
	for _, parameter := range strings.Split(parameters, ";") {
		key, value, found := strings.Cut(parameter, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || parsed < 0 || parsed > 1 {
			return "", 0, false
		}
		quality = parsed
	}
	return name, quality, true
}
