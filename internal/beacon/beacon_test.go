package beacon_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/mtrenker/pacapulse/internal/beacon"
)

// request issues one request against a fresh handler and returns the recorder.
func request(t *testing.T, method, acceptEncoding string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, beacon.Path, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	beacon.NewHandler().ServeHTTP(recorder, req)
	return recorder
}

// TestAssetIsServedWithEveryNormativeHeader pins the response contract from
// the collection protocol. Each header is load-bearing: the content type and
// nosniff stop the asset being reinterpreted, the cache directives are what
// keep a tracked page from refetching it on every navigation, the validator
// makes revalidation cheap, Vary keeps a shared cache from handing compressed
// bytes to a client that cannot read them, and the resource policy is what
// permits a cross-origin page to load it at all.
func TestAssetIsServedWithEveryNormativeHeader(t *testing.T) {
	t.Parallel()

	resp := request(t, http.MethodGet, "", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}

	want := map[string]string{
		"Content-Type":                 "application/javascript; charset=utf-8",
		"Cache-Control":                "public, max-age=3600, stale-while-revalidate=86400",
		"Vary":                         "Accept-Encoding",
		"X-Content-Type-Options":       "nosniff",
		"Cross-Origin-Resource-Policy": "cross-origin",
	}
	for name, value := range want {
		if got := resp.Header().Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if resp.Header().Get("ETag") == "" {
		t.Error("ETag is missing; conditional requests would refetch the whole asset")
	}
	if got := resp.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q for an unnegotiated request, want none", got)
	}
	if got := resp.Header().Get("Content-Length"); got != strconv.Itoa(len(beacon.Source())) {
		t.Errorf("Content-Length = %q, want %d", got, len(beacon.Source()))
	}
}

// TestServedAssetIsTheProductionSource is the byte-identity guarantee: what a
// reviewer reads in the repository is exactly what a tracked page executes,
// with no build step, minifier, or template in between.
func TestServedAssetIsTheProductionSource(t *testing.T) {
	t.Parallel()

	onDisk, err := os.ReadFile("beacon.js")
	if err != nil {
		t.Fatalf("read the production source: %v", err)
	}
	if !bytes.Equal(onDisk, beacon.Source()) {
		t.Fatal("the embedded asset differs from beacon.js on disk")
	}

	resp := request(t, http.MethodGet, "", nil)
	if !bytes.Equal(resp.Body.Bytes(), onDisk) {
		t.Error("the served identity body differs from the production source")
	}
}

func TestGzipRepresentationDecodesToTheProductionSource(t *testing.T) {
	t.Parallel()

	resp := request(t, http.MethodGet, "gzip", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	if got := resp.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := resp.Header().Get("Content-Length"); got != strconv.Itoa(resp.Body.Len()) {
		t.Errorf("Content-Length = %q, want %d", got, resp.Body.Len())
	}

	reader, err := gzip.NewReader(bytes.NewReader(resp.Body.Bytes()))
	if err != nil {
		t.Fatalf("open the gzip body: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read the gzip body: %v", err)
	}
	if !bytes.Equal(decoded, beacon.Source()) {
		t.Error("the gzip representation does not decode to the production source")
	}
}

// TestRepresentationsHaveDistinctValidators keeps a shared cache honest. Two
// byte-different representations sharing one strong validator is how a client
// ends up with compressed bytes labelled identity, or the reverse.
func TestRepresentationsHaveDistinctValidators(t *testing.T) {
	t.Parallel()

	identity := request(t, http.MethodGet, "identity", nil).Header().Get("ETag")
	compressed := request(t, http.MethodGet, "gzip", nil).Header().Get("ETag")

	if identity == "" || compressed == "" {
		t.Fatalf("missing validator: identity = %q, gzip = %q", identity, compressed)
	}
	if identity == compressed {
		t.Errorf("both representations use validator %q", identity)
	}
}

// TestValidatorIsDeterministic proves the validator is derived from content
// rather than from process start, so a restarted or replicated instance does
// not invalidate every cached copy.
func TestValidatorIsDeterministic(t *testing.T) {
	t.Parallel()

	first := beacon.NewHandler()
	second := beacon.NewHandler()

	for _, encoding := range []string{"", "gzip"} {
		firstRecorder, secondRecorder := httptest.NewRecorder(), httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, beacon.Path, nil)
		if encoding != "" {
			req.Header.Set("Accept-Encoding", encoding)
		}
		first.ServeHTTP(firstRecorder, req)
		second.ServeHTTP(secondRecorder, req)

		if a, b := firstRecorder.Header().Get("ETag"), secondRecorder.Header().Get("ETag"); a != b {
			t.Errorf("Accept-Encoding %q: validators differ between instances: %q and %q", encoding, a, b)
		}
	}
}

func TestConditionalRequestIsNotModified(t *testing.T) {
	t.Parallel()

	tests := map[string]string{"identity": "", "gzip": "gzip"}
	for name, encoding := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			etag := request(t, http.MethodGet, encoding, nil).Header().Get("ETag")
			resp := request(t, http.MethodGet, encoding, map[string]string{"If-None-Match": etag})

			if resp.Code != http.StatusNotModified {
				t.Fatalf("status = %d, want %d", resp.Code, http.StatusNotModified)
			}
			if resp.Body.Len() != 0 {
				t.Errorf("304 carried %d body bytes", resp.Body.Len())
			}
			for header, want := range map[string]string{
				"ETag":          etag,
				"Cache-Control": "public, max-age=3600, stale-while-revalidate=86400",
				"Vary":          "Accept-Encoding",
			} {
				if got := resp.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}

// TestValidatorOfAnotherRepresentationDoesNotMatch is the other half of the
// distinct-validator rule: a client holding the identity copy must be sent
// the whole gzip representation rather than a 304 it cannot decode.
func TestValidatorOfAnotherRepresentationDoesNotMatch(t *testing.T) {
	t.Parallel()

	identityTag := request(t, http.MethodGet, "identity", nil).Header().Get("ETag")
	resp := request(t, http.MethodGet, "gzip", map[string]string{"If-None-Match": identityTag})

	if resp.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.Code, http.StatusOK)
	}
}

func TestAcceptEncodingNegotiation(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		accept   string
		absent   bool
		wantCode int
		wantGzip bool
	}{
		"no header serves identity":        {absent: true, wantCode: http.StatusOK},
		"empty value serves identity":      {accept: "", wantCode: http.StatusOK},
		"gzip":                             {accept: "gzip", wantCode: http.StatusOK, wantGzip: true},
		"gzip among others":                {accept: "br, gzip, deflate", wantCode: http.StatusOK, wantGzip: true},
		"uppercase coding name":            {accept: "GZIP", wantCode: http.StatusOK, wantGzip: true},
		"whitespace and quality":           {accept: " gzip ; q=1.0 ", wantCode: http.StatusOK, wantGzip: true},
		"wildcard":                         {accept: "*", wantCode: http.StatusOK, wantGzip: true},
		"gzip refused":                     {accept: "gzip;q=0", wantCode: http.StatusOK},
		"gzip refused under a wildcard":    {accept: "*, gzip;q=0", wantCode: http.StatusOK},
		"identity preferred over gzip":     {accept: "gzip;q=0.5, identity;q=0.9", wantCode: http.StatusOK},
		"gzip preferred over identity":     {accept: "gzip;q=0.9, identity;q=0.5", wantCode: http.StatusOK, wantGzip: true},
		"only an unknown coding":           {accept: "br", wantCode: http.StatusOK},
		"unknown coding cannot exclude":    {accept: "br;q=1.0, gzip;q=0", wantCode: http.StatusOK},
		"identity refused leaves gzip":     {accept: "identity;q=0, gzip", wantCode: http.StatusOK, wantGzip: true},
		"identity refused alone":           {accept: "identity;q=0", wantCode: http.StatusNotAcceptable},
		"everything refused":               {accept: "*;q=0", wantCode: http.StatusNotAcceptable},
		"wildcard refused, gzip permitted": {accept: "*;q=0, gzip;q=1", wantCode: http.StatusOK, wantGzip: true},
		"malformed quality is dropped":     {accept: "gzip;q=high", wantCode: http.StatusOK},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, beacon.Path, nil)
			if !tc.absent {
				req.Header.Set("Accept-Encoding", tc.accept)
			}
			resp := httptest.NewRecorder()
			beacon.NewHandler().ServeHTTP(resp, req)

			if resp.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", resp.Code, tc.wantCode)
			}
			if got := resp.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("Vary = %q, want %q", got, "Accept-Encoding")
			}
			if tc.wantCode != http.StatusOK {
				return
			}
			gotGzip := resp.Header().Get("Content-Encoding") == "gzip"
			if gotGzip != tc.wantGzip {
				t.Errorf("gzip = %t, want %t (Content-Encoding %q)", gotGzip, tc.wantGzip, resp.Header().Get("Content-Encoding"))
			}
			if !gotGzip && !bytes.Equal(resp.Body.Bytes(), beacon.Source()) {
				t.Error("the identity body is not the production source")
			}
		})
	}
}

func TestHeadServesHeadersWithoutABody(t *testing.T) {
	t.Parallel()

	resp := request(t, http.MethodHead, "", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	if resp.Body.Len() != 0 {
		t.Errorf("HEAD returned %d body bytes", resp.Body.Len())
	}
	if got := resp.Header().Get("Content-Length"); got != strconv.Itoa(len(beacon.Source())) {
		t.Errorf("Content-Length = %q, want %d", got, len(beacon.Source()))
	}
}

func TestUnsupportedMethodsAreRejected(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			resp := request(t, method, "", nil)
			if resp.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", resp.Code, http.StatusMethodNotAllowed)
			}
			if got := resp.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
			}
			if resp.Body.Len() != 0 {
				t.Errorf("rejection carried %d body bytes", resp.Body.Len())
			}
		})
	}
}

// TestSourceIsAnIndependentCopy stops a caller from mutating what every
// tracked page in the world then downloads.
func TestSourceIsAnIndependentCopy(t *testing.T) {
	t.Parallel()

	copied := beacon.Source()
	copied[0] = 'X'

	if beacon.Source()[0] == 'X' {
		t.Error("Source returns the embedded backing array rather than a copy")
	}
}
