package storage

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixture identifiers. They are canonical lowercase UUIDs, because the store
// treats the text form as the idempotency key and refuses any other spelling.
const (
	testOrgID    = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	testSiteID   = "1b1b1b1b-1b1b-4b1b-8b1b-1b1b1b1b1b1b"
	absentSiteID = "2c2c2c2c-2c2c-4c2c-8c2c-2c2c2c2c2c2c"
	testDay      = "2026-03-14"
	testPath     = "/pricing"
	testReferrer = "news.example.com"
	testCampaign = "spring-launch"
)

// newStore opens an initialized database that already has the organization and
// site an accepted page view has to reference. Site administration belongs to a
// later issue, so the fixture is created directly in SQL.
func newStore(t *testing.T) *Database {
	t.Helper()

	db := open(t, filepath.Join(t.TempDir(), "data"))
	addSite(t, db, testSiteID)
	return db
}

func addSite(t *testing.T, db *Database, siteID string) {
	t.Helper()

	exec(t, db, "INSERT OR IGNORE INTO organizations (id, name, created_at_ms) VALUES (?, ?, 0)",
		testOrgID, "Example Organization")
	exec(t, db, "INSERT INTO sites (id, organization_id, name, collection_key, canonical_host, created_at_ms) "+
		"VALUES (?, ?, ?, ?, ?, 0)",
		siteID, testOrgID, "Example Site", "key-"+siteID, "example.com")
}

// samplePageView is one fully populated page view: every optional dimension
// present, so that a test which wants a dimension absent has to say so.
func samplePageView() PageView {
	return PageView{
		SiteID:         testSiteID,
		EventID:        eventID(1),
		ReceivedAt:     fixedTime,
		Path:           testPath,
		ReferrerHost:   testReferrer,
		CampaignSource: "newsletter",
		CampaignMedium: "email",
		CampaignName:   testCampaign,
	}
}

// eventID builds a canonical UUID from a counter, so that a test can deliver
// distinct events without a random source.
func eventID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
}

// visitorKey builds a distinct full-length visitor key.
func visitorKey(n byte) []byte {
	key := make([]byte, visitorKeyBytes)
	for i := range key {
		key[i] = n
	}
	return key
}

// record delivers one page view and fails the test if it is not accepted.
func record(t *testing.T, db *Database, view PageView) PageViewResult {
	t.Helper()

	result, err := db.RecordPageView(context.Background(), view)
	if err != nil {
		t.Fatalf("RecordPageView() error = %v, want nil", err)
	}
	return result
}

// statTables are the aggregate families; visitorTables are their short-lived
// deduplication partners.
var (
	statTables    = []string{"daily_site_stats", "daily_page_stats", "daily_referrer_stats", "daily_campaign_stats"}
	visitorTables = []string{"daily_site_visitors", "daily_page_visitors", "daily_referrer_visitors", "daily_campaign_visitors"}
)

// snapshot reads every count an accepted event can change: the ledger, each
// aggregate family's rows and totals, and each visitor table. Comparing two
// snapshots is how the tests prove a failed event kept nothing at all, rather
// than only that it reported an error.
func snapshot(t *testing.T, db *Database) map[string]int {
	t.Helper()

	counts := map[string]int{
		"page_view_events": query[int](t, db, "SELECT count(*) FROM page_view_events"),
	}
	for _, table := range statTables {
		counts[table] = query[int](t, db, "SELECT count(*) FROM "+table)
		counts[table+".page_views"] = query[int](t, db, "SELECT coalesce(sum(page_views), 0) FROM "+table)
		counts[table+".visitors"] = query[int](t, db, "SELECT coalesce(sum(visitors), 0) FROM "+table)
	}
	for _, table := range visitorTables {
		counts[table] = query[int](t, db, "SELECT count(*) FROM "+table)
	}
	return counts
}

func assertUnchanged(t *testing.T, db *Database, before map[string]int) {
	t.Helper()

	after := snapshot(t, db)
	if !maps.Equal(before, after) {
		t.Errorf("the database changed: before = %v, after = %v", before, after)
	}
}

// TestRecordedPageViewUpdatesEveryApplicableFamily is the ordinary accepted
// event: one ledger row, one increment in every family the page view has a
// dimension for, and nothing in the families it does not.
func TestRecordedPageViewUpdatesEveryApplicableFamily(t *testing.T) {
	t.Parallel()

	t.Run("every dimension present", func(t *testing.T) {
		t.Parallel()

		db := newStore(t)
		view := samplePageView()
		view.VisitorKey = visitorKey(1)

		result := record(t, db, view)
		if result.Duplicate {
			t.Error("Duplicate = true for a first delivery")
		}
		if result.Day != testDay {
			t.Errorf("Day = %q, want %q", result.Day, testDay)
		}

		if got := query[int](t, db, "SELECT count(*) FROM page_view_events WHERE site_id = ? AND event_id = ?",
			view.SiteID, view.EventID); got != 1 {
			t.Errorf("ledger rows = %d, want 1", got)
		}
		ledgerPath := query[string](t, db, "SELECT path FROM page_view_events WHERE event_id = ?", view.EventID)
		if ledgerPath != testPath {
			t.Errorf("ledger path = %q, want %q", ledgerPath, testPath)
		}

		for _, table := range statTables {
			views := query[int](t, db, "SELECT coalesce(sum(page_views), 0) FROM "+table)
			visitors := query[int](t, db, "SELECT coalesce(sum(visitors), 0) FROM "+table)
			if views != 1 || visitors != 1 {
				t.Errorf("%s: page_views = %d, visitors = %d, want 1 and 1", table, views, visitors)
			}
		}
		for _, table := range visitorTables {
			if got := query[int](t, db, "SELECT count(*) FROM "+table); got != 1 {
				t.Errorf("%s rows = %d, want 1", table, got)
			}
		}
	})

	t.Run("no referrer and no campaign", func(t *testing.T) {
		t.Parallel()

		db := newStore(t)
		view := samplePageView()
		view.ReferrerHost = ""
		view.CampaignSource, view.CampaignMedium, view.CampaignName = "", "", ""
		view.VisitorKey = visitorKey(1)

		record(t, db, view)

		for _, table := range []string{"daily_site_stats", "daily_page_stats"} {
			if got := query[int](t, db, "SELECT count(*) FROM "+table); got != 1 {
				t.Errorf("%s rows = %d, want 1", table, got)
			}
		}
		for _, table := range []string{
			"daily_referrer_stats", "daily_campaign_stats",
			"daily_referrer_visitors", "daily_campaign_visitors",
		} {
			if got := query[int](t, db, "SELECT count(*) FROM "+table); got != 0 {
				t.Errorf("%s rows = %d, want 0 for a page view with neither dimension", table, got)
			}
		}
		// The ledger keeps "no referrer" distinct from "the empty hostname".
		if got := query[int](t, db, "SELECT count(*) FROM page_view_events WHERE referrer_host IS NULL"); got != 1 {
			t.Errorf("ledger rows with a NULL referrer = %d, want 1", got)
		}
	})

	t.Run("partial campaign tuple still counts", func(t *testing.T) {
		t.Parallel()

		db := newStore(t)
		view := samplePageView()
		view.CampaignMedium, view.CampaignName = "", ""

		record(t, db, view)

		if got := query[int](t, db, "SELECT count(*) FROM daily_campaign_stats"); got != 1 {
			t.Errorf("campaign rows = %d, want 1 for a tuple with one non-empty member", got)
		}
		got := query[string](t, db, "SELECT campaign_medium || '|' || campaign_name FROM daily_campaign_stats")
		if got != "|" {
			t.Errorf("absent campaign members = %q, want empty strings", got)
		}
	})
}

// TestSuccessOnlyAfterCommit injects a failure at each stage of the event
// transaction and proves the whole event is undone, not just the stage that
// failed. It is the atomicity guarantee the data model states as "any
// validation, SQL, count-update, or commit failure records no part of the
// event".
func TestSuccessOnlyAfterCommit(t *testing.T) {
	t.Parallel()

	stages := []string{
		"ledger", "site", "page", "referrer", "campaign", "visitors", "commit",
		// Between a visitor's deduplication row and the count it justifies.
		// Losing the increment alone would silently drop that visitor for the
		// rest of the day, because the next delivery would find the key already
		// recorded and count nothing.
		"site visitor increment", "page visitor increment",
		"referrer visitor increment", "campaign visitor increment",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()

			db := newStore(t)
			// A committed event first, so the stages that update an existing
			// row are exercised as well as the ones that create it.
			first := samplePageView()
			first.VisitorKey = visitorKey(1)
			record(t, db, first)
			before := snapshot(t, db)

			injected := errors.New("injected stage failure")
			failAt(t, db, stage, injected)

			second := samplePageView()
			second.EventID = eventID(2)
			second.VisitorKey = visitorKey(2)
			if _, err := db.RecordPageView(context.Background(), second); !errors.Is(err, injected) {
				t.Fatalf("RecordPageView() error = %v, want the injected failure", err)
			}
			assertUnchanged(t, db, before)
		})
	}
}

// failAt makes one transaction stage fail. The hook is unexported and nil in
// production; see failureHook.
func failAt(t *testing.T, db *Database, stage string, err error) {
	t.Helper()

	db.fail.Store(&failureHook{fn: func(reached string) error {
		if reached == stage {
			return err
		}
		return nil
	}})
	t.Cleanup(func() { db.fail.Store(nil) })
}

// TestDuplicateEventIsASuccessfulNoOp covers the idempotency key. The first
// accepted event wins, whatever a repeat says.
func TestDuplicateEventIsASuccessfulNoOp(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		repeat func(PageView) PageView
	}{
		{
			name:   "identical payload",
			repeat: func(view PageView) PageView { return view },
		},
		{
			name: "different payload",
			repeat: func(view PageView) PageView {
				view.Path = "/somewhere-else"
				view.ReferrerHost = "other.example.com"
				view.CampaignSource = "banner"
				view.CampaignMedium = "display"
				view.CampaignName = "autumn"
				view.VisitorKey = visitorKey(9)
				view.ReceivedAt = view.ReceivedAt.Add(time.Hour)
				return view
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := newStore(t)
			view := samplePageView()
			view.VisitorKey = visitorKey(1)
			record(t, db, view)
			before := snapshot(t, db)

			result := record(t, db, tc.repeat(view))
			if !result.Duplicate {
				t.Error("Duplicate = false for a repeated event id")
			}
			if result.Day != testDay {
				t.Errorf("Day = %q, want %q", result.Day, testDay)
			}
			assertUnchanged(t, db, before)

			// The ledger still holds the first accepted payload.
			if got := query[string](t, db, "SELECT path FROM page_view_events WHERE event_id = ?", view.EventID); got != testPath {
				t.Errorf("ledger path = %q, want the first accepted %q", got, testPath)
			}
		})
	}
}

// TestConcurrentDuplicateDeliveryCountsOnce is the retrying beacon: the same
// event arriving many times at once must produce one measurement.
func TestConcurrentDuplicateDeliveryCountsOnce(t *testing.T) {
	t.Parallel()

	db := newStore(t)
	view := samplePageView()
	view.VisitorKey = visitorKey(1)

	const deliveries = 16
	results := make(chan PageViewResult, deliveries)
	failures := make(chan error, deliveries)
	for i := range deliveries {
		go func() {
			// Every delivery repeats the same event id and differs in the rest
			// of the payload, the way a retry with a re-read document would.
			repeat := view
			repeat.Path = fmt.Sprintf("/retry-%d", i)
			result, err := db.RecordPageView(context.Background(), repeat)
			if err != nil {
				failures <- err
				return
			}
			results <- result
		}()
	}

	accepted := 0
	for range deliveries {
		select {
		case err := <-failures:
			t.Fatalf("RecordPageView() error = %v, want nil", err)
		case result := <-results:
			if !result.Duplicate {
				accepted++
			}
		}
	}
	if accepted != 1 {
		t.Errorf("non-duplicate results = %d, want exactly 1", accepted)
	}

	if got := query[int](t, db, "SELECT count(*) FROM page_view_events"); got != 1 {
		t.Errorf("ledger rows = %d, want 1", got)
	}
	for _, table := range statTables {
		views := query[int](t, db, "SELECT coalesce(sum(page_views), 0) FROM "+table)
		visitors := query[int](t, db, "SELECT coalesce(sum(visitors), 0) FROM "+table)
		if views != 1 || visitors != 1 {
			t.Errorf("%s: page_views = %d, visitors = %d, want 1 and 1", table, views, visitors)
		}
	}
}

// TestRejectedPageViewsWriteNothing covers the contract's whole rejection
// surface. Every case must be typed, must leave the database untouched, and
// must not echo the value it rejected.
func TestRejectedPageViewsWriteNothing(t *testing.T) {
	t.Parallel()

	oversizedPath := "/" + strings.Repeat("a", maxPathBytes)
	oversizedHost := strings.Repeat("h", maxReferrerHostBytes+1)
	oversizedCampaign := strings.Repeat("c", maxCampaignValueBytes+1)

	cases := []struct {
		name    string
		mutate  func(*PageView)
		want    error
		secrets []string
	}{
		{
			name:    "site id is not a UUID",
			mutate:  func(v *PageView) { v.SiteID = "not-a-uuid" },
			want:    ErrInvalidPageView,
			secrets: []string{"not-a-uuid"},
		},
		{
			name:    "site id is upper case",
			mutate:  func(v *PageView) { v.SiteID = strings.ToUpper(testSiteID) },
			want:    ErrInvalidPageView,
			secrets: []string{strings.ToUpper(testSiteID)},
		},
		{
			name:    "site id is brace wrapped",
			mutate:  func(v *PageView) { v.SiteID = "{" + testSiteID + "}" },
			want:    ErrInvalidPageView,
			secrets: []string{testSiteID},
		},
		{
			name:    "event id is truncated",
			mutate:  func(v *PageView) { v.EventID = eventID(1)[:35] },
			want:    ErrInvalidPageView,
			secrets: []string{eventID(1)[:35]},
		},
		{
			name:   "event id has a non-hexadecimal digit",
			mutate: func(v *PageView) { v.EventID = "0000000g-0000-4000-8000-000000000001" },
			want:   ErrInvalidPageView,
		},
		{
			name:   "site id is the nil UUID",
			mutate: func(v *PageView) { v.SiteID = "00000000-0000-0000-0000-000000000000" },
			want:   ErrInvalidPageView,
		},
		{
			name:   "event id is the nil UUID",
			mutate: func(v *PageView) { v.EventID = "00000000-0000-0000-0000-000000000000" },
			want:   ErrInvalidPageView,
		},
		{
			// A v1 UUID is derived from a MAC address and a timestamp, which is
			// the opposite of the random identifier an event id has to be.
			name:   "event id is version 1",
			mutate: func(v *PageView) { v.EventID = "00000000-0000-1000-8000-000000000001" },
			want:   ErrInvalidPageView,
		},
		{
			name:   "event id is version 7",
			mutate: func(v *PageView) { v.EventID = "00000000-0000-7000-8000-000000000001" },
			want:   ErrInvalidPageView,
		},
		{
			name:   "event id has a Microsoft variant nibble",
			mutate: func(v *PageView) { v.EventID = "00000000-0000-4000-c000-000000000001" },
			want:   ErrInvalidPageView,
		},
		{
			name:   "event id has a reserved variant nibble",
			mutate: func(v *PageView) { v.EventID = "00000000-0000-4000-f000-000000000001" },
			want:   ErrInvalidPageView,
		},
		{
			name:   "event id is upper case",
			mutate: func(v *PageView) { v.EventID = "00000000-0000-4000-8000-00000000000A" },
			want:   ErrInvalidPageView,
		},
		{
			name:   "receive time is before the epoch",
			mutate: func(v *PageView) { v.ReceivedAt = time.UnixMilli(0).Add(-time.Second) },
			want:   ErrInvalidPageView,
		},
		{
			name:   "receive time is the zero time",
			mutate: func(v *PageView) { v.ReceivedAt = time.Time{} },
			want:   ErrInvalidPageView,
		},
		{
			name:   "receive time has no four-digit year",
			mutate: func(v *PageView) { v.ReceivedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
			want:   ErrInvalidPageView,
		},
		{
			name:   "path is empty",
			mutate: func(v *PageView) { v.Path = "" },
			want:   ErrInvalidPageView,
		},
		{
			name:    "path does not begin with a slash",
			mutate:  func(v *PageView) { v.Path = "pricing" },
			want:    ErrInvalidPageView,
			secrets: []string{"pricing"},
		},
		{
			name:    "path carries a query",
			mutate:  func(v *PageView) { v.Path = "/search?q=secret" },
			want:    ErrInvalidPageView,
			secrets: []string{"secret"},
		},
		{
			name:    "path carries a fragment",
			mutate:  func(v *PageView) { v.Path = "/docs#section" },
			want:    ErrInvalidPageView,
			secrets: []string{"section"},
		},
		{
			name:    "path exceeds 512 bytes",
			mutate:  func(v *PageView) { v.Path = oversizedPath },
			want:    ErrInvalidPageView,
			secrets: []string{oversizedPath},
		},
		{
			name:   "path is not valid UTF-8",
			mutate: func(v *PageView) { v.Path = "/" + string([]byte{0xff, 0xfe}) },
			want:   ErrInvalidPageView,
		},
		{
			name:   "multi-byte path is measured in bytes",
			mutate: func(v *PageView) { v.Path = "/" + strings.Repeat("ü", maxPathBytes/2) },
			want:   ErrInvalidPageView,
		},
		{
			name:    "referrer host exceeds 253 bytes",
			mutate:  func(v *PageView) { v.ReferrerHost = oversizedHost },
			want:    ErrInvalidPageView,
			secrets: []string{oversizedHost},
		},
		{
			name:   "referrer host is not valid UTF-8",
			mutate: func(v *PageView) { v.ReferrerHost = string([]byte{0xff}) },
			want:   ErrInvalidPageView,
		},
		{
			name:    "campaign source exceeds 100 bytes",
			mutate:  func(v *PageView) { v.CampaignSource = oversizedCampaign },
			want:    ErrInvalidPageView,
			secrets: []string{oversizedCampaign},
		},
		{
			name:    "campaign medium exceeds 100 bytes",
			mutate:  func(v *PageView) { v.CampaignMedium = oversizedCampaign },
			want:    ErrInvalidPageView,
			secrets: []string{oversizedCampaign},
		},
		{
			name:    "campaign name exceeds 100 bytes",
			mutate:  func(v *PageView) { v.CampaignName = oversizedCampaign },
			want:    ErrInvalidPageView,
			secrets: []string{oversizedCampaign},
		},
		{
			name:   "visitor key is too short",
			mutate: func(v *PageView) { v.VisitorKey = make([]byte, visitorKeyBytes-1) },
			want:   ErrInvalidPageView,
		},
		{
			name:   "visitor key is too long",
			mutate: func(v *PageView) { v.VisitorKey = make([]byte, visitorKeyBytes+1) },
			want:   ErrInvalidPageView,
		},
		{
			name:   "visitor key is empty rather than absent",
			mutate: func(v *PageView) { v.VisitorKey = []byte{} },
			want:   ErrInvalidPageView,
		},
		{
			name:    "site does not exist",
			mutate:  func(v *PageView) { v.SiteID = absentSiteID },
			want:    ErrUnknownSite,
			secrets: []string{absentSiteID},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := newStore(t)
			before := snapshot(t, db)

			view := samplePageView()
			tc.mutate(&view)
			_, err := db.RecordPageView(context.Background(), view)
			if !errors.Is(err, tc.want) {
				t.Fatalf("RecordPageView() error = %v, want %v", err, tc.want)
			}
			assertUnchanged(t, db, before)
			assertPrivate(t, err, tc.secrets...)
		})
	}
}

// TestEveryRFC4122VariantIsAccepted is the other half of the UUID contract: the
// four variant nibbles a real random UUIDv4 can carry must all be accepted, so
// the check rejects the wrong shape rather than three quarters of the right one.
func TestEveryRFC4122VariantIsAccepted(t *testing.T) {
	t.Parallel()

	db := newStore(t)
	for i, variant := range []string{"8", "9", "a", "b"} {
		view := samplePageView()
		view.EventID = "00000000-0000-4000-" + variant + "000-00000000000" + fmt.Sprint(i)
		if !isCanonicalUUIDv4(view.EventID) {
			t.Fatalf("fixture %q is not a canonical UUIDv4", view.EventID)
		}
		record(t, db, view)
	}
	if got := query[int](t, db, "SELECT count(*) FROM page_view_events"); got != 4 {
		t.Errorf("ledger rows = %d, want 4", got)
	}
}

// TestDriverFailuresDoNotLeakSchemaDetail is the privacy rule applied to the
// one source that does not obey it on its own.
//
// SQLite's message for a failed statement names the table, the column, and the
// constraint. That is exactly what a caller offering a page view must not learn,
// so an ingestion failure carries the numeric result code instead. The failure
// here is a real one: a counter already at the maximum integer, so that adding
// one produces a REAL the STRICT column refuses.
func TestDriverFailuresDoNotLeakSchemaDetail(t *testing.T) {
	t.Parallel()

	const maxInteger = int64(9223372036854775807)
	cases := []struct {
		name string
		// plant puts the database into the state that makes the next page view
		// fail inside the transaction.
		plant func(*testing.T, *Database)
		// leaked is the schema detail SQLite would otherwise have disclosed.
		leaked []string
	}{
		{
			name: "site aggregate",
			plant: func(t *testing.T, db *Database) {
				exec(t, db, "INSERT INTO daily_site_stats (site_id, day, page_views, visitors) "+
					"VALUES (?, ?, ?, 0)", testSiteID, testDay, maxInteger)
			},
			leaked: []string{"daily_site_stats", "page_views", "INTEGER column", "REAL"},
		},
		{
			name: "page aggregate",
			plant: func(t *testing.T, db *Database) {
				exec(t, db, "INSERT INTO daily_page_stats "+
					"(site_id, day, path, overflow, page_views, visitors) VALUES (?, ?, ?, 0, ?, 0)",
					testSiteID, testDay, testPath, maxInteger)
			},
			leaked: []string{"daily_page_stats", "page_views", "INTEGER column", "REAL"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := newStore(t)
			tc.plant(t, db)
			before := snapshot(t, db)

			_, err := db.RecordPageView(context.Background(), samplePageView())
			if !errors.Is(err, ErrWrite) {
				t.Fatalf("RecordPageView() error = %v, want %v", err, ErrWrite)
			}
			// The result code is kept: it is what an operator looks the
			// condition up by, and it names nothing.
			if !strings.Contains(err.Error(), "sqlite result code ") {
				t.Errorf("error = %v, want it to report the SQLite result code", err)
			}
			assertPrivate(t, err, tc.leaked...)
			// And the failed page view kept nothing, as every other failure does.
			assertUnchanged(t, db, before)
		})
	}
}

// TestDeletedSiteIsRejected covers the site that existed when the page view was
// accepted and no longer does when it is written.
func TestDeletedSiteIsRejected(t *testing.T) {
	t.Parallel()

	db := newStore(t)
	record(t, db, samplePageView())
	exec(t, db, "DELETE FROM sites WHERE id = ?", testSiteID)

	before := snapshot(t, db)
	view := samplePageView()
	view.EventID = eventID(2)
	_, err := db.RecordPageView(context.Background(), view)
	if !errors.Is(err, ErrUnknownSite) {
		t.Fatalf("RecordPageView() error = %v, want %v", err, ErrUnknownSite)
	}
	assertUnchanged(t, db, before)
	assertPrivate(t, err, testSiteID, view.EventID)
}

// TestAggregateDayComesFromTheReceiveInstant proves the day is derived in UTC,
// inside the store, from the one server instant — including a request the
// server accepts on the other side of midnight from the visitor's clock.
func TestAggregateDayComesFromTheReceiveInstant(t *testing.T) {
	t.Parallel()

	berlin := time.FixedZone("CET", 2*60*60)
	cases := []struct {
		name       string
		receivedAt time.Time
		want       string
	}{
		{
			name:       "one millisecond before UTC midnight",
			receivedAt: time.Date(2026, 3, 14, 23, 59, 59, int(999*time.Millisecond), time.UTC),
			want:       "2026-03-14",
		},
		{
			name:       "exactly UTC midnight",
			receivedAt: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
			want:       "2026-03-15",
		},
		{
			name:       "one millisecond after UTC midnight",
			receivedAt: time.Date(2026, 3, 15, 0, 0, 0, int(time.Millisecond), time.UTC),
			want:       "2026-03-15",
		},
		{
			name:       "a non-UTC instant is converted, not reinterpreted",
			receivedAt: time.Date(2026, 3, 15, 0, 30, 0, 0, berlin),
			want:       "2026-03-14",
		},
		{
			name:       "the Unix epoch itself is accepted",
			receivedAt: time.UnixMilli(0).UTC(),
			want:       "1970-01-01",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := newStore(t)
			view := samplePageView()
			view.EventID = eventID(i + 1)
			view.ReceivedAt = tc.receivedAt

			result := record(t, db, view)
			if result.Day != tc.want {
				t.Errorf("Day = %q, want %q", result.Day, tc.want)
			}
			for _, table := range statTables {
				if got := query[string](t, db, "SELECT day FROM "+table); got != tc.want {
					t.Errorf("%s day = %q, want %q", table, got, tc.want)
				}
			}
			day := query[string](t, db, "SELECT day FROM page_view_events")
			receivedAt := query[int64](t, db, "SELECT received_at_ms FROM page_view_events")
			if day != tc.want {
				t.Errorf("ledger day = %q, want %q", day, tc.want)
			}
			if want := tc.receivedAt.UnixMilli(); receivedAt != want {
				t.Errorf("received_at_ms = %d, want %d", receivedAt, want)
			}
		})
	}
}

// TestVisitorCountsOncePerSiteDayDimension is the daily-visitor estimate: page
// views keep accumulating while a returning visitor is counted once in each
// site/day/dimension row.
func TestVisitorCountsOncePerSiteDayDimension(t *testing.T) {
	t.Parallel()

	t.Run("two views by one visitor on one page", func(t *testing.T) {
		t.Parallel()

		db := newStore(t)
		for i := range 2 {
			view := samplePageView()
			view.EventID = eventID(i + 1)
			view.VisitorKey = visitorKey(1)
			record(t, db, view)
		}

		for _, table := range statTables {
			views := query[int](t, db, "SELECT coalesce(sum(page_views), 0) FROM "+table)
			visitors := query[int](t, db, "SELECT coalesce(sum(visitors), 0) FROM "+table)
			if views != 2 || visitors != 1 {
				t.Errorf("%s: page_views = %d, visitors = %d, want 2 and 1", table, views, visitors)
			}
		}
	})

	t.Run("one visitor on two pages", func(t *testing.T) {
		t.Parallel()

		db := newStore(t)
		for i, path := range []string{"/one", "/two"} {
			view := samplePageView()
			view.EventID = eventID(i + 1)
			view.Path = path
			view.VisitorKey = visitorKey(1)
			record(t, db, view)
		}

		if got := query[int](t, db, "SELECT visitors FROM daily_site_stats"); got != 1 {
			t.Errorf("site visitors = %d, want 1", got)
		}
		// Page visitor counts are intentionally non-additive: the same person
		// is one visitor in each page row and one visitor for the site.
		if got := query[int](t, db, "SELECT coalesce(sum(visitors), 0) FROM daily_page_stats"); got != 2 {
			t.Errorf("summed page visitors = %d, want 2 across two page rows", got)
		}
		if got := query[int](t, db, "SELECT count(*) FROM daily_page_stats"); got != 2 {
			t.Errorf("page rows = %d, want 2", got)
		}
	})

	t.Run("two visitors sharing every dimension", func(t *testing.T) {
		t.Parallel()

		db := newStore(t)
		for i := range 2 {
			view := samplePageView()
			view.EventID = eventID(i + 1)
			view.VisitorKey = visitorKey(byte(i + 1))
			record(t, db, view)
		}

		for _, table := range statTables {
			if got := query[int](t, db, "SELECT coalesce(sum(visitors), 0) FROM "+table); got != 2 {
				t.Errorf("%s visitors = %d, want 2", table, got)
			}
		}
	})

	t.Run("one visitor using two referrers and two campaigns", func(t *testing.T) {
		t.Parallel()

		db := newStore(t)
		for i, referrer := range []string{"a.example.com", "b.example.com"} {
			view := samplePageView()
			view.EventID = eventID(i + 1)
			view.ReferrerHost = referrer
			view.CampaignSource = fmt.Sprintf("source-%d", i)
			view.VisitorKey = visitorKey(1)
			record(t, db, view)
		}

		if got := query[int](t, db, "SELECT visitors FROM daily_site_stats"); got != 1 {
			t.Errorf("site visitors = %d, want 1", got)
		}
		for _, table := range []string{"daily_referrer_stats", "daily_campaign_stats"} {
			rows := query[int](t, db, "SELECT count(*) FROM "+table)
			visitors := query[int](t, db, "SELECT coalesce(sum(visitors), 0) FROM "+table)
			if rows != 2 || visitors != 2 {
				t.Errorf("%s: rows = %d, summed visitors = %d, want 2 and 2", table, rows, visitors)
			}
		}
	})

	t.Run("the same visitor on two days", func(t *testing.T) {
		t.Parallel()

		db := newStore(t)
		for i, instant := range []time.Time{
			time.Date(2026, 3, 14, 23, 59, 59, 0, time.UTC),
			time.Date(2026, 3, 15, 0, 0, 1, 0, time.UTC),
		} {
			view := samplePageView()
			view.EventID = eventID(i + 1)
			view.ReceivedAt = instant
			view.VisitorKey = visitorKey(1)
			record(t, db, view)
		}

		if got := query[int](t, db, "SELECT count(*) FROM daily_site_stats"); got != 2 {
			t.Errorf("site rows = %d, want one per UTC day", got)
		}
		// Summing daily visitor counts produces visitor-days, by design.
		if got := query[int](t, db, "SELECT coalesce(sum(visitors), 0) FROM daily_site_stats"); got != 2 {
			t.Errorf("summed site visitors = %d, want 2 visitor-days", got)
		}
	})

	t.Run("no visitor key changes no visitor state", func(t *testing.T) {
		t.Parallel()

		db := newStore(t)
		for i := range 3 {
			view := samplePageView()
			view.EventID = eventID(i + 1)
			record(t, db, view)
		}

		for _, table := range statTables {
			views := query[int](t, db, "SELECT coalesce(sum(page_views), 0) FROM "+table)
			visitors := query[int](t, db, "SELECT coalesce(sum(visitors), 0) FROM "+table)
			if views != 3 || visitors != 0 {
				t.Errorf("%s: page_views = %d, visitors = %d, want 3 and 0", table, views, visitors)
			}
		}
		for _, table := range visitorTables {
			if got := query[int](t, db, "SELECT count(*) FROM "+table); got != 0 {
				t.Errorf("%s rows = %d, want 0 without a visitor key", table, got)
			}
		}
	})
}

// TestCardinalityIsBoundedByOverflowRows covers the limits that make forged
// dimensions cost a bounded number of rows: the first value past a family's
// limit lands in that family's one reserved overflow row, an already-known
// value keeps its exact row, and one visitor across several overflow values
// counts once.
func TestCardinalityIsBoundedByOverflowRows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		table string
		limit int
		// dimension applies the nth distinct value to a page view.
		dimension func(*PageView, int)
		// overflowShape is the reserved row's dimension columns.
		overflowShape string
	}{
		{
			name:          "paths",
			table:         "daily_page_stats",
			limit:         maxPathsPerSiteDay,
			dimension:     func(v *PageView, n int) { v.Path = fmt.Sprintf("/p/%d", n) },
			overflowShape: "path = ''",
		},
		{
			name:          "referrer hosts",
			table:         "daily_referrer_stats",
			limit:         maxReferrersPerSiteDay,
			dimension:     func(v *PageView, n int) { v.ReferrerHost = fmt.Sprintf("r%d.example.com", n) },
			overflowShape: "referrer_host = ''",
		},
		{
			name:      "campaign tuples",
			table:     "daily_campaign_stats",
			limit:     maxCampaignsPerSiteDay,
			dimension: func(v *PageView, n int) { v.CampaignSource = fmt.Sprintf("s%d", n) },
			overflowShape: "campaign_source = '' AND campaign_medium = '' " +
				"AND campaign_name = ''",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := newStore(t)
			next := 0
			deliver := func(mutate func(*PageView, int), value int, key []byte) {
				t.Helper()
				next++
				view := samplePageView()
				view.EventID = eventID(next)
				view.VisitorKey = key
				mutate(&view, value)
				record(t, db, view)
			}

			// Fill the family to one below its limit. The rows are seeded
			// directly so the test stays a test of the resolution rule rather
			// than of several thousand commits.
			seed(t, db, tc.table, tc.limit-1, tc.dimension)

			// The last value below the limit still gets its own exact row…
			deliver(tc.dimension, tc.limit, nil)
			if got := query[int](t, db, "SELECT count(*) FROM "+tc.table+" WHERE overflow = 0"); got != tc.limit {
				t.Fatalf("exact rows = %d, want the limit %d", got, tc.limit)
			}
			if got := query[int](t, db, "SELECT count(*) FROM "+tc.table+" WHERE overflow = 1"); got != 0 {
				t.Fatalf("overflow rows = %d, want 0 before the limit is exceeded", got)
			}

			// …and the first value past it lands in the reserved overflow row.
			visitor := visitorKey(1)
			deliver(tc.dimension, tc.limit+1, visitor)
			// A second, different overflowing value from the same visitor adds
			// a page view but not a visitor: overflowing values share one row,
			// so its (stat_id, visitor_key) is already there.
			deliver(tc.dimension, tc.limit+2, visitor)
			// And a value the day already knows keeps its exact row even now.
			deliver(tc.dimension, 1, nil)

			overflowRows := query[int](t, db, "SELECT count(*) FROM "+tc.table+" WHERE overflow = 1")
			if overflowRows != 1 {
				t.Fatalf("overflow rows = %d, want exactly 1", overflowRows)
			}
			views := query[int](t, db, "SELECT page_views FROM "+tc.table+" WHERE overflow = 1")
			visitors := query[int](t, db, "SELECT visitors FROM "+tc.table+" WHERE overflow = 1")
			if views != 2 || visitors != 1 {
				t.Errorf("overflow row: page_views = %d, visitors = %d, want 2 and 1", views, visitors)
			}
			shaped := query[int](t, db, "SELECT count(*) FROM "+tc.table+" WHERE overflow = 1 AND "+tc.overflowShape)
			if shaped != 1 {
				t.Errorf("the overflow row does not have the reserved empty dimension shape")
			}

			// The known value's exact row absorbed its page view rather than
			// overflowing with the new ones.
			total := query[int](t, db, "SELECT count(*) FROM "+tc.table)
			if want := tc.limit + 1; total != want {
				t.Errorf("%s rows = %d, want the ceiling of %d", tc.table, total, want)
			}
			knownViews := query[int](t, db, "SELECT sum(page_views) FROM "+tc.table+" WHERE overflow = 0")
			if want := tc.limit + 1; knownViews != want {
				t.Errorf("exact-row page views = %d, want %d", knownViews, want)
			}

			// The site total has no dimension, so it counts every event.
			if got := query[int](t, db, "SELECT count(*) FROM daily_site_stats"); got != 1 {
				t.Errorf("site rows = %d, want 1", got)
			}
			if got := query[int](t, db, "SELECT page_views FROM daily_site_stats"); got != next {
				t.Errorf("site page views = %d, want %d", got, next)
			}
		})
	}
}

// seed creates count distinct non-overflow rows for the fixture site and day by
// replaying the store's own resolution for values 1..count, in one transaction.
func seed(t *testing.T, db *Database, table string, count int, dimension func(*PageView, int)) {
	t.Helper()

	handle, err := db.handle()
	if err != nil {
		t.Fatalf("handle() error = %v", err)
	}
	ctx := context.Background()
	tx, err := handle.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	for n := 1; n <= count; n++ {
		// The base is the delivered page view, so a seeded dimension and a
		// recorded one describe the same row.
		view := samplePageView()
		dimension(&view, n)
		var statement string
		var args []any
		switch table {
		case "daily_page_stats":
			statement = "INSERT INTO daily_page_stats (site_id, day, path, overflow, page_views, visitors) " +
				"VALUES (?, ?, ?, 0, 1, 0)"
			args = []any{testSiteID, testDay, view.Path}
		case "daily_referrer_stats":
			statement = "INSERT INTO daily_referrer_stats (site_id, day, referrer_host, overflow, page_views, visitors) " +
				"VALUES (?, ?, ?, 0, 1, 0)"
			args = []any{testSiteID, testDay, view.ReferrerHost}
		case "daily_campaign_stats":
			statement = "INSERT INTO daily_campaign_stats " +
				"(site_id, day, campaign_source, campaign_medium, campaign_name, overflow, page_views, visitors) " +
				"VALUES (?, ?, ?, ?, ?, 0, 1, 0)"
			args = []any{testSiteID, testDay, view.CampaignSource, view.CampaignMedium, view.CampaignName}
		default:
			t.Fatalf("unknown table %q", table)
		}
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("seed %s: %v", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed transaction: %v", err)
	}
}

// TestCardinalityCeilingHoldsForEveryFamily proves the whole per-site/day
// envelope at once: one site row, 2,001 page rows, 501 referrer rows, and 501
// campaign rows, whatever an attacker sends.
func TestCardinalityCeilingHoldsForEveryFamily(t *testing.T) {
	t.Parallel()

	db := newStore(t)
	seed(t, db, "daily_page_stats", maxPathsPerSiteDay, func(v *PageView, n int) { v.Path = fmt.Sprintf("/p/%d", n) })
	seed(t, db, "daily_referrer_stats", maxReferrersPerSiteDay,
		func(v *PageView, n int) { v.ReferrerHost = fmt.Sprintf("r%d.example.com", n) })
	seed(t, db, "daily_campaign_stats", maxCampaignsPerSiteDay,
		func(v *PageView, n int) { v.CampaignSource = fmt.Sprintf("s%d", n) })

	// Twenty further forged events, each with three brand-new dimensions.
	for i := range 20 {
		view := samplePageView()
		view.EventID = eventID(i + 1)
		view.Path = fmt.Sprintf("/forged/%d", i)
		view.ReferrerHost = fmt.Sprintf("forged%d.example.com", i)
		view.CampaignSource = fmt.Sprintf("forged-%d", i)
		view.VisitorKey = visitorKey(byte(i))
		record(t, db, view)
	}

	for _, want := range []struct {
		table string
		rows  int
	}{
		{"daily_site_stats", 1},
		{"daily_page_stats", maxPathsPerSiteDay + 1},
		{"daily_referrer_stats", maxReferrersPerSiteDay + 1},
		{"daily_campaign_stats", maxCampaignsPerSiteDay + 1},
	} {
		if got := query[int](t, db, "SELECT count(*) FROM "+want.table); got != want.rows {
			t.Errorf("%s rows = %d, want %d", want.table, got, want.rows)
		}
	}
	for _, table := range []string{"daily_page_stats", "daily_referrer_stats", "daily_campaign_stats"} {
		if got := query[int](t, db, "SELECT count(*) FROM "+table+" WHERE overflow = 1"); got != 1 {
			t.Errorf("%s overflow rows = %d, want 1", table, got)
		}
		if got := query[int](t, db, "SELECT page_views FROM "+table+" WHERE overflow = 1"); got != 20 {
			t.Errorf("%s overflow page views = %d, want 20", table, got)
		}
		if got := query[int](t, db, "SELECT visitors FROM "+table+" WHERE overflow = 1"); got != 20 {
			t.Errorf("%s overflow visitors = %d, want 20 distinct visitors", table, got)
		}
	}
}

// TestSeparateSitesAndDaysDoNotShareLimits proves the bound is per site and
// day, so one abused site cannot deny another its exact dimensions.
func TestSeparateSitesAndDaysDoNotShareLimits(t *testing.T) {
	t.Parallel()

	db := newStore(t)
	addSite(t, db, absentSiteID)
	seed(t, db, "daily_referrer_stats", maxReferrersPerSiteDay,
		func(v *PageView, n int) { v.ReferrerHost = fmt.Sprintf("r%d.example.com", n) })

	// Another site, same day.
	other := samplePageView()
	other.SiteID = absentSiteID
	other.EventID = eventID(1)
	other.ReferrerHost = "fresh.example.com"
	record(t, db, other)

	// The same site, the next day.
	tomorrow := samplePageView()
	tomorrow.EventID = eventID(2)
	tomorrow.ReceivedAt = fixedTime.Add(24 * time.Hour)
	tomorrow.ReferrerHost = "fresh.example.com"
	record(t, db, tomorrow)

	if got := query[int](t, db, "SELECT count(*) FROM daily_referrer_stats WHERE overflow = 1"); got != 0 {
		t.Errorf("overflow rows = %d, want 0: the limit is per site and day", got)
	}
	if got := query[int](t, db,
		"SELECT count(*) FROM daily_referrer_stats WHERE referrer_host = ?", "fresh.example.com"); got != 2 {
		t.Errorf("exact rows for the new host = %d, want one per site/day", got)
	}
}

// assertPrivate fails when a diagnostic carries a value it was produced from,
// the SQL that produced it, or the database's location. These errors reach the
// operator log and, through it, whatever a later collector reports.
func assertPrivate(t *testing.T, err error, secrets ...string) {
	t.Helper()

	if err == nil {
		t.Fatal("expected an error to inspect")
	}
	message := err.Error()
	forbidden := append([]string{
		testSiteID, testOrgID, testPath, testReferrer, testCampaign,
		DatabaseFileName, LockFileName,
		// SQL, and the schema it would name. An ingestion caller is told what
		// went wrong, not what the database is made of.
		"INSERT INTO", "SELECT ", "UPDATE ", "ON CONFLICT", "RETURNING",
		"daily_", "page_view_events", "schema_migrations", "sqlite_",
		"constraint failed",
	}, secrets...)
	for _, value := range forbidden {
		if value == "" {
			continue
		}
		if strings.Contains(message, value) {
			t.Errorf("error message contains %q: %s", value, message)
		}
	}
}
