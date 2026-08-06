package collector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mtrenker/pacapulse/internal/storage"
)

const fixtureSecret = "0123456789abcdef0123456789abcdef"

type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixedClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fixedClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type fakeBoundary struct {
	mu       sync.Mutex
	sites    map[string]storage.CollectionSite
	disabled map[string]bool
	views    []storage.PageView
	events   map[string]bool
}

func (f *fakeBoundary) ResolveCollectionSite(_ context.Context, key string) (storage.CollectionSite, error) {
	if f.disabled[key] {
		return storage.CollectionSite{}, storage.ErrUnknownSite
	}
	site, ok := f.sites[key]
	if !ok {
		return storage.CollectionSite{}, storage.ErrUnknownSite
	}
	return site, nil
}
func (f *fakeBoundary) RecordPageView(_ context.Context, view storage.PageView) (storage.PageViewResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch view.EventID {
	case "8d399973-dc50-41de-98e8-f0a1b730d3c7":
		return storage.PageViewResult{}, storage.ErrQueueFull
	case "5ca43adf-2280-4e80-b5fa-4a3b796dcb80":
		return storage.PageViewResult{}, storage.ErrUnavailable
	}
	key := view.SiteID + "/" + view.EventID
	if f.events[key] {
		return storage.PageViewResult{Duplicate: true}, nil
	}
	f.events[key] = true
	view.VisitorKey = bytes.Clone(view.VisitorKey)
	f.views = append(f.views, view)
	return storage.PageViewResult{}, nil
}

type protocolFixtures struct {
	Site            fixtureSite   `json:"site"`
	DNTOverrideSite fixtureSite   `json:"dnt_override_site"`
	DevelopmentSite fixtureSite   `json:"development_site"`
	DisabledSite    fixtureSite   `json:"disabled_site"`
	ServerRequests  []fixtureCase `json:"server_requests"`
	InvalidRequests []fixtureCase `json:"invalid_requests"`
}
type fixtureSite struct {
	CollectionKey     string   `json:"collection_key"`
	CanonicalHost     string   `json:"canonical_host"`
	AllowedHosts      []string `json:"allowed_hosts"`
	CollectionEnabled bool     `json:"collection_enabled"`
	CollectDNT        bool     `json:"collect_dnt"`
}
type fixtureCase struct {
	Name    string `json:"name"`
	Request struct {
		Method             string            `json:"method"`
		ContentType        string            `json:"content_type"`
		Origin             *string           `json:"origin"`
		Headers            map[string]string `json:"headers"`
		Payload            map[string]any    `json:"payload"`
		RawBody            string            `json:"raw_body"`
		RawBodyBase64      string            `json:"raw_body_base64"`
		GeneratedBodyBytes int               `json:"generated_body_bytes"`
	} `json:"request"`
	Precondition string `json:"precondition"`
	Expected     struct {
		Status             int     `json:"status"`
		CORSOrigin         *string `json:"cors_origin"`
		Storage            string  `json:"storage"`
		StoredReferrerHost *string `json:"stored_referrer_host"`
		StoredCampaign     any     `json:"stored_campaign"`
	} `json:"expected"`
}

func loadFixtures(t *testing.T) protocolFixtures {
	t.Helper()
	body, err := os.ReadFile("../../docs/architecture/collection-protocol-fixtures.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixtures protocolFixtures
	if err := json.Unmarshal(body, &fixtures); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	return fixtures
}

func newFixtureHandler(fixtures protocolFixtures) (*Handler, *fakeBoundary, *fixedClock) {
	boundary := &fakeBoundary{sites: make(map[string]storage.CollectionSite), disabled: make(map[string]bool), events: make(map[string]bool)}
	ids := []string{
		"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444",
	}
	for index, config := range []fixtureSite{fixtures.Site, fixtures.DNTOverrideSite, fixtures.DevelopmentSite, fixtures.DisabledSite} {
		boundary.sites[config.CollectionKey] = storage.CollectionSite{ID: ids[index], CanonicalHost: config.CanonicalHost, AllowedHosts: config.AllowedHosts, CollectionEnabled: config.CollectionEnabled, CollectDNT: config.CollectDNT}
		if !config.CollectionEnabled {
			boundary.disabled[config.CollectionKey] = true
		}
	}
	clock := &fixedClock{now: time.Date(2026, 3, 14, 23, 59, 59, 0, time.UTC)}
	return New(Options{Resolver: boundary, Recorder: boundary, VisitorSecret: []byte(fixtureSecret), Clock: clock}), boundary, clock
}

func fixtureBody(t *testing.T, test fixtureCase) []byte {
	t.Helper()
	switch {
	case test.Request.GeneratedBodyBytes > 0:
		return bytes.Repeat([]byte{'x'}, test.Request.GeneratedBodyBytes)
	case test.Request.RawBodyBase64 != "":
		body, err := base64.StdEncoding.DecodeString(test.Request.RawBodyBase64)
		if err != nil {
			t.Fatalf("decode raw body: %v", err)
		}
		return body
	case test.Request.RawBody != "":
		return []byte(test.Request.RawBody)
	default:
		payload := test.Request.Payload
		if generated, ok := payload["generated_path_utf8_bytes"].(float64); ok {
			delete(payload, "generated_path_utf8_bytes")
			payload["path"] = "/" + strings.Repeat("p", int(generated)-1)
		}
		if generated, ok := payload["generated_utm_campaign_utf8_bytes"].(float64); ok {
			delete(payload, "generated_utm_campaign_utf8_bytes")
			payload["utm_campaign"] = strings.Repeat("c", int(generated))
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return body
	}
}

func TestContractFixturesDriveCollector(t *testing.T) {
	fixtures := loadFixtures(t)
	cases := append(append([]fixtureCase{}, fixtures.ServerRequests...), fixtures.InvalidRequests...)
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			handler, boundary, clock := newFixtureHandler(fixtures)
			if strings.Contains(test.Precondition, "minimal accepted") {
				boundary.events["11111111-1111-4111-8111-111111111111/550e8400-e29b-41d4-a716-446655440000"] = true
			}
			if strings.Contains(test.Precondition, "site token bucket") {
				handler.sites.buckets["11111111-1111-4111-8111-111111111111"] = &bucket{tokens: 0, last: clock.Now()}
			}
			method := test.Request.Method
			if method == "" {
				method = http.MethodPost
			}
			req := httptest.NewRequest(method, PageViewsPath, bytes.NewReader(fixtureBody(t, test)))
			req.RemoteAddr = "192.0.2.20:4321"
			if test.Request.ContentType != "" {
				req.Header.Set("Content-Type", test.Request.ContentType)
			}
			if test.Request.Origin != nil {
				req.Header.Set("Origin", *test.Request.Origin)
			}
			for name, value := range test.Request.Headers {
				req.Header.Set(name, value)
			}
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)

			if resp.Code != test.Expected.Status {
				t.Errorf("status = %d, want %d", resp.Code, test.Expected.Status)
			}
			if resp.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", resp.Body.String())
			}
			if got := resp.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
			wantCORS := ""
			if test.Expected.CORSOrigin != nil {
				wantCORS = *test.Expected.CORSOrigin
			}
			if got := resp.Header().Get("Access-Control-Allow-Origin"); got != wantCORS {
				t.Errorf("CORS origin = %q, want %q", got, wantCORS)
			}
			if wantCORS != "" && resp.Header().Get("Vary") != "Origin" {
				t.Errorf("Vary = %q, want Origin", resp.Header().Get("Vary"))
			}
			boundary.mu.Lock()
			views := append([]storage.PageView(nil), boundary.views...)
			boundary.mu.Unlock()
			switch test.Expected.Storage {
			case "committed":
				if len(views) != 1 {
					t.Fatalf("writes = %d, want 1", len(views))
				}
			case "not_called", "duplicate_no_op", "retryable_unavailable":
				if len(views) != 0 {
					t.Errorf("writes = %d, want 0", len(views))
				}
			}
			if len(views) == 1 && test.Expected.StoredReferrerHost == nil && strings.Contains(test.Name, "same-site") && views[0].ReferrerHost != "" {
				t.Errorf("stored referrer = %q, want absent", views[0].ReferrerHost)
			}
			if len(views) == 1 && strings.Contains(test.Name, "oversized optional") && views[0].CampaignName != "" {
				t.Errorf("stored campaign = %q, want absent", views[0].CampaignName)
			}
		})
	}
}

func TestUnsupportedMethodsAreEmptyAndNeverCORSApproved(t *testing.T) {
	fixtures := loadFixtures(t)
	handler, boundary, _ := newFixtureHandler(fixtures)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodOptions} {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(method, PageViewsPath, nil)
		req.Header.Set("Origin", "https://shop.example")
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusMethodNotAllowed || resp.Body.Len() != 0 {
			t.Errorf("%s response = %d %q", method, resp.Code, resp.Body.String())
		}
		if resp.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s received CORS approval", method)
		}
	}
	if len(boundary.views) != 0 {
		t.Errorf("writes = %d, want 0", len(boundary.views))
	}
}

func TestStrictJSONRejectsDuplicateTrailingAndMalformedUTF8Independently(t *testing.T) {
	valid := `{"v":1,"site":"pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ","event_id":"550e8400-e29b-41d4-a716-446655440000","path":"/"}`
	cases := map[string][]byte{
		"duplicate":       []byte(strings.Replace(valid, `"path":"/"`, `"path":"/","path":"/two"`, 1)),
		"trailing":        []byte(valid + `{}`),
		"malformed UTF-8": append([]byte(valid[:len(valid)-2]), 0xff, '"', '}'),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePayload(body); err == nil {
				t.Fatal("parsePayload() error = nil")
			}
		})
	}
}

func TestFixedHandlerRateLimitsAndCORSVisibility(t *testing.T) {
	fixtures := loadFixtures(t)

	t.Run("instance burst", func(t *testing.T) {
		handler, _, _ := newFixtureHandler(fixtures)
		for index := 0; index <= 500; index++ {
			req := httptest.NewRequest(http.MethodPost, PageViewsPath, strings.NewReader("x"))
			req.Header.Set("Content-Type", "text/plain")
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			want := http.StatusBadRequest
			if index == 500 {
				want = http.StatusTooManyRequests
			}
			if resp.Code != want {
				t.Fatalf("request %d status = %d, want %d", index+1, resp.Code, want)
			}
			if resp.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("instance-limited response received CORS approval")
			}
		}
	})

	for name, tc := range map[string]struct {
		count  int
		remote string
	}{
		"site burst":   {201, "unavailable"},
		"source burst": {51, "192.0.2.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			handler, _, _ := newFixtureHandler(fixtures)
			for index := 1; index <= tc.count; index++ {
				body := fmt.Sprintf(`{"v":1,"site":"pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ","event_id":"00000000-0000-4000-8000-%012d","path":"/"}`, index)
				req := httptest.NewRequest(http.MethodPost, PageViewsPath, strings.NewReader(body))
				req.Header.Set("Content-Type", "text/plain")
				req.Header.Set("Origin", "https://shop.example")
				req.RemoteAddr = tc.remote
				resp := httptest.NewRecorder()
				handler.ServeHTTP(resp, req)
				want := http.StatusNoContent
				if index == tc.count {
					want = http.StatusTooManyRequests
				}
				if resp.Code != want {
					t.Fatalf("request %d status = %d, want %d", index, resp.Code, want)
				}
				if resp.Header().Get("Access-Control-Allow-Origin") != "https://shop.example" {
					t.Fatalf("request %d omitted CORS approval", index)
				}
			}
		})
	}
}

func TestUnavailableAddressStillCommitsWithoutVisitorKey(t *testing.T) {
	fixtures := loadFixtures(t)
	handler, boundary, _ := newFixtureHandler(fixtures)
	req := httptest.NewRequest(http.MethodPost, PageViewsPath, strings.NewReader(`{"v":1,"site":"pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ","event_id":"12345678-1234-4234-8234-123456789abc","path":"/"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "https://shop.example")
	req.RemoteAddr = "unavailable"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.Code)
	}
	if len(boundary.views) != 1 || boundary.views[0].VisitorKey != nil {
		t.Errorf("stored views = %+v, want one without a visitor key", boundary.views)
	}
}

func TestStorageErrorsCollapseToEmpty503(t *testing.T) {
	fixtures := loadFixtures(t)
	handler, _, _ := newFixtureHandler(fixtures)
	req := httptest.NewRequest(http.MethodPost, PageViewsPath, strings.NewReader(`{"v":1,"site":"pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ","event_id":"5ca43adf-2280-4e80-b5fa-4a3b796dcb80","path":"/"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "https://shop.example")
	req.RemoteAddr = "192.0.2.1:1"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusServiceUnavailable || resp.Body.Len() != 0 {
		t.Errorf("response = %d %q", resp.Code, resp.Body.String())
	}
}
