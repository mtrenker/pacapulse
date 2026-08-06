// Package collector implements the strict, privacy-minimizing page-view HTTP
// boundary. Raw request data is reduced to storage.PageView here and is never
// offered to storage, logs, or another queue.
package collector

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/mtrenker/pacapulse/internal/storage"
)

// PageViewsPath is the stable public collector endpoint.
const PageViewsPath = "/api/v1/pageviews"

// Resolver exposes only enabled collection policy to the handler.
type Resolver interface {
	ResolveCollectionSite(context.Context, string) (storage.CollectionSite, error)
}

// Recorder accepts only the post-sanitization storage shape.
type Recorder interface {
	RecordPageView(context.Context, storage.PageView) (storage.PageViewResult, error)
}

// Clock supplies deterministic limiter and analytics time in tests.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Options supplies the collector's narrow runtime dependencies.
type Options struct {
	Resolver          Resolver
	Recorder          Recorder
	VisitorSecret     []byte
	TrustedProxyCIDRs []netip.Prefix
	Clock             Clock
}

// Handler enforces the complete page-view collection boundary.
type Handler struct {
	resolver Resolver
	recorder Recorder
	secret   []byte
	trusted  []netip.Prefix
	clock    Clock

	instanceMu sync.Mutex
	instance   bucket
	sites      *keyedLimiter
	sources    *sourceLimiter
}

// New creates an independent collector with fixed instance, site, and source
// limits.
func New(opts Options) *Handler {
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Handler{
		resolver: opts.Resolver,
		recorder: opts.Recorder,
		secret:   append([]byte(nil), opts.VisitorSecret...),
		trusted:  append([]netip.Prefix(nil), opts.TrustedProxyCIDRs...),
		clock:    clock,
		sites:    newKeyedLimiter(50, 200),
		sources:  newSourceLimiter(),
	}
}

// SourceLimiterEvictions returns the one intentionally unlabeled collector
// metric. It reveals only aggregate limiter churn and no source or site key.
func (h *Handler) SourceLimiterEvictions() uint64 { return h.sources.evictionCount() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	h.instanceMu.Lock()
	allowed := h.instance.allow(h.clock.Now(), 100, 500)
	h.instanceMu.Unlock()
	if !allowed {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if !acceptedMediaType(r.Header.Get("Content-Type")) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	candidate, err := parsePayload(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Optional dimensions fail closed to absence rather than rejecting the
	// mandatory event. They are repeated after privacy and source gates before
	// storage, as the processing-order contract requires.
	if normalized, ok := normalizeReferrerHost(candidate.ReferrerHost); ok {
		candidate.ReferrerHost = normalized
	} else {
		candidate.ReferrerHost = ""
	}
	candidate.UTMSource = normalizeCampaign(candidate.UTMSource)
	candidate.UTMMedium = normalizeCampaign(candidate.UTMMedium)
	candidate.UTMCampaign = normalizeCampaign(candidate.UTMCampaign)

	site, err := h.resolver.ResolveCollectionSite(r.Context(), candidate.Site)
	if errors.Is(err, storage.ErrUnknownSite) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if !site.CollectionEnabled {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	hosts := configuredHosts(site)
	origin, ok := acceptedOrigin(r.Header.Values("Origin"), hosts, isLoopbackSite(site.CanonicalHost))
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")

	if !h.sites.allow(site.ID, h.clock.Now()) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if headerReportsOne(r.Header.Values("Sec-GPC")) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if (headerReportsOne(r.Header.Values("DNT")) || candidate.DNT) && !site.CollectDNT {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	address, hasAddress := clientAddress(r, h.trusted)
	if hasAddress {
		sourceAt := h.clock.Now()
		if !h.sources.allow(sourceKey(h.secret, site.ID, sourceAt, address), sourceAt) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
	}

	path, ok := canonicalizePath(candidate.Path)
	if !ok || !isCanonicalUUIDv4(candidate.EventID) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	referrer := ""
	if normalized, valid := normalizeReferrerHost(candidate.ReferrerHost); valid {
		if _, sameSite := hosts[normalized]; !sameSite {
			referrer = normalized
		}
	}
	source := normalizeCampaign(candidate.UTMSource)
	medium := normalizeCampaign(candidate.UTMMedium)
	campaign := normalizeCampaign(candidate.UTMCampaign)

	// This is the one analytics instant. Its UTC day scopes the visitor HMAC,
	// and the exact same value reaches storage for timestamp and aggregate day.
	receivedAt := h.clock.Now()
	var visitor []byte
	if hasAddress {
		browser, device := classifyUserAgent(r.Header.Get("User-Agent"))
		key := visitorKey(h.secret, site.ID, receivedAt, address, browser, device)
		visitor = key[:]
	}

	_, err = h.recorder.RecordPageView(r.Context(), storage.PageView{
		SiteID: site.ID, EventID: candidate.EventID, ReceivedAt: receivedAt,
		Path: path, ReferrerHost: referrer,
		CampaignSource: source, CampaignMedium: medium, CampaignName: campaign,
		VisitorKey: visitor,
	})
	for i := range visitor {
		visitor[i] = 0
	}
	if err != nil {
		// Every storage failure is intentionally one public category. In
		// particular no driver detail or internal validation reason is exposed.
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func headerReportsOne(values []string) bool {
	for _, value := range values {
		for _, member := range strings.Split(value, ",") {
			if strings.TrimSpace(member) == "1" {
				return true
			}
		}
	}
	return false
}

func acceptedMediaType(value string) bool {
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil || !strings.EqualFold(mediaType, "text/plain") || len(params) > 1 {
		return false
	}
	for name, value := range params {
		if !strings.EqualFold(name, "charset") || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

func isLoopbackSite(canonicalHost string) bool {
	host, ok := normalizeHost(canonicalHost, true)
	if !ok {
		return false
	}
	if host == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func configuredHosts(site storage.CollectionSite) map[string]struct{} {
	hosts := make(map[string]struct{}, len(site.AllowedHosts)+1)
	for _, value := range append([]string{site.CanonicalHost}, site.AllowedHosts...) {
		if host, ok := normalizeHost(value, true); ok {
			hosts[host] = struct{}{}
		}
	}
	return hosts
}
