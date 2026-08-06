package storage

import (
	"strings"
	"time"
	"unicode/utf8"
)

// PageView is one already-sanitized page view offered to the store.
//
// The contract is deliberately post-sanitization. There is no field for a
// client address, a user-agent header, a full referrer URL, a query string, or
// any other raw request input: deciding what a request means belongs to the
// collector, and the values that reach SQLite are the bounded canonical ones
// docs/architecture/data-model.md describes.
//
// The zero value of every optional field means "absent", so a caller that never
// learned a dimension does not have to model its absence twice.
type PageView struct {
	// SiteID is the site's random UUIDv4 in canonical lowercase text form.
	SiteID string
	// EventID is the beacon's per-page-view random UUIDv4 in canonical
	// lowercase text form. Together with SiteID it is the idempotency key: a
	// repeat is a successful no-op, whatever the rest of the payload says.
	EventID string
	// ReceivedAt is the one server instant the collector captured. Both the
	// stored millisecond timestamp and the UTC aggregate day are derived from
	// it, inside this package — the browser does not choose analytics time and
	// no caller supplies a day.
	ReceivedAt time.Time
	// Path is the canonical URL path: at most 512 UTF-8 bytes, beginning with
	// '/', carrying no query or fragment.
	Path string
	// ReferrerHost is the external referrer's normalized hostname, or empty
	// when the page view has no usable external referrer.
	ReferrerHost string
	// CampaignSource, CampaignMedium, and CampaignName are the normalized utm_*
	// values, each at most 100 UTF-8 bytes. The tuple counts as present when at
	// least one member is non-empty; absent members are empty strings.
	CampaignSource string
	CampaignMedium string
	CampaignName   string
	// VisitorKey is the full 32-byte site- and day-scoped visitor HMAC, or nil
	// when no trustworthy client address was available. A page view without one
	// is still counted; it simply increments no visitor estimate.
	VisitorKey []byte
}

// PageViewResult reports what a successfully committed call did.
type PageViewResult struct {
	// Day is the UTC aggregate day, in YYYY-MM-DD form, the event was counted
	// on. It is derived from ReceivedAt and returned so that a caller can log
	// or report the day without recomputing the boundary rule.
	Day string
	// Duplicate is true when this (site, event) pair had already been recorded.
	// The stored measurement is the first one that was accepted; nothing was
	// counted again, and the repeated payload was not merged.
	Duplicate bool
}

// Bounds the ingestion contract enforces before a transaction is opened. They
// mirror migration 1's CHECK constraints, so a rejected page view is rejected
// with a typed error rather than by SQLite in the middle of a write.
const (
	uuidTextLength        = 36
	maxPathBytes          = 512
	maxReferrerHostBytes  = 253
	maxCampaignValueBytes = 100
	// visitorKeyBytes is the full HMAC-SHA256 output. Truncation would save
	// little and add no useful property at this scale.
	visitorKeyBytes = 32
	// maxReceivedAtSeconds is 9999-12-31T23:59:59Z. The schema's day constraint
	// is a four-digit year, so a later instant has no representable day.
	maxReceivedAtSeconds = 253402300799
	// dayLayout is the canonical ISO UTC day the aggregates are keyed by.
	dayLayout = "2006-01-02"
)

// hasCampaign reports whether the campaign tuple is present. Missing members
// are empty strings, and a tuple with at least one non-empty member is what
// creates campaign aggregate and visitor rows.
func (v PageView) hasCampaign() bool {
	return v.CampaignSource != "" || v.CampaignMedium != "" || v.CampaignName != ""
}

// referrerValue renders the nullable ledger column: SQL NULL for a page view
// with no usable external referrer, so that "no referrer" and "the empty
// hostname" cannot be confused in the 30-day ledger.
func (v PageView) referrerValue() any {
	if v.ReferrerHost == "" {
		return nil
	}
	return v.ReferrerHost
}

// day returns the UTC aggregate day derived from the receive instant, and the
// millisecond timestamp stored beside it. Both come from the same value, so a
// row can never be filed under a day its own timestamp does not fall in.
func (v PageView) day() (int64, string) {
	milli := v.ReceivedAt.UnixMilli()
	return milli, time.UnixMilli(milli).UTC().Format(dayLayout)
}

// validate rejects a page view that does not satisfy the ingestion contract.
//
// It runs before any transaction is opened, so a rejected page view leaves no
// row, no aggregate, and no visitor key behind, and it never has to be undone.
//
// Every diagnostic names the field and the expectation and never the value: the
// values in question are a site's identifiers, its visitors' paths, referrers,
// campaigns, and a secret-keyed visitor estimate, and this error reaches an
// operator log.
func validatePageView(view PageView) error {
	const stage = "validate page view"

	switch {
	case !isCanonicalUUIDv4(view.SiteID):
		return describe(ErrInvalidPageView, stage,
			"site id must be a canonical lowercase 36-character UUIDv4")
	case !isCanonicalUUIDv4(view.EventID):
		return describe(ErrInvalidPageView, stage,
			"event id must be a canonical lowercase 36-character UUIDv4")
	}

	seconds := view.ReceivedAt.Unix()
	switch {
	case seconds < 0:
		return describe(ErrInvalidPageView, stage,
			"receive time must not be earlier than the Unix epoch")
	case seconds > maxReceivedAtSeconds:
		return describe(ErrInvalidPageView, stage,
			"receive time must not be later than the year 9999")
	}

	switch {
	case len(view.Path) == 0 || len(view.Path) > maxPathBytes:
		return describe(ErrInvalidPageView, stage,
			"path must be between 1 and 512 UTF-8 bytes")
	case !utf8.ValidString(view.Path):
		return describe(ErrInvalidPageView, stage, "path must be valid UTF-8")
	case view.Path[0] != '/':
		return describe(ErrInvalidPageView, stage, "path must begin with '/'")
	case strings.ContainsAny(view.Path, "?#"):
		return describe(ErrInvalidPageView, stage,
			"path must carry no query or fragment, so it must contain no '?' or '#'")
	}

	if view.ReferrerHost != "" {
		switch {
		case len(view.ReferrerHost) > maxReferrerHostBytes:
			return describe(ErrInvalidPageView, stage,
				"referrer host must be between 1 and 253 UTF-8 bytes")
		case !utf8.ValidString(view.ReferrerHost):
			return describe(ErrInvalidPageView, stage, "referrer host must be valid UTF-8")
		}
	}

	for _, campaign := range []struct {
		field string
		value string
	}{
		{"campaign source", view.CampaignSource},
		{"campaign medium", view.CampaignMedium},
		{"campaign name", view.CampaignName},
	} {
		switch {
		case len(campaign.value) > maxCampaignValueBytes:
			return describe(ErrInvalidPageView, stage,
				campaign.field+" must be at most 100 UTF-8 bytes")
		case !utf8.ValidString(campaign.value):
			return describe(ErrInvalidPageView, stage, campaign.field+" must be valid UTF-8")
		}
	}

	if view.VisitorKey != nil && len(view.VisitorKey) != visitorKeyBytes {
		return describe(ErrInvalidPageView, stage,
			"visitor key must be absent or exactly 32 bytes")
	}
	return nil
}

// Positions of the two nibbles RFC 4122 reserves inside the canonical text
// form xxxxxxxx-xxxx-Vxxx-Nxxx-xxxxxxxxxxxx.
const (
	uuidVersionIndex = 14
	uuidVariantIndex = 19
)

// isCanonicalUUIDv4 reports whether s is the canonical lowercase RFC 4122 text
// form of a version 4 UUID: 8-4-4-4-12 lowercase hexadecimal digits separated
// by hyphens, with the version nibble '4' and the variant nibble in 8..b.
//
// The form is checked rather than parsed because the database stores the text:
// accepting an upper-case or brace-wrapped spelling would let the same UUID
// arrive as two different idempotency keys.
//
// The version and variant are checked because the data model specifies random
// UUIDv4 for both identifiers. Insisting on it rejects the nil UUID, a
// time-ordered v1 that would carry a MAC address and a timestamp, and anything
// else that is shaped like a UUID without being the random identifier a site ID
// and an event ID are supposed to be.
func isCanonicalUUIDv4(s string) bool {
	if len(s) != uuidTextLength {
		return false
	}
	for i := range uuidTextLength {
		c := s[i]
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
	if s[uuidVersionIndex] != '4' {
		return false
	}
	switch s[uuidVariantIndex] {
	case '8', '9', 'a', 'b':
		return true
	}
	return false
}
