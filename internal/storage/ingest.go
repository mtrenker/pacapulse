package storage

import (
	"context"
	"database/sql"
	"errors"
)

// Fixed per-site/day aggregate cardinality limits.
//
// They bound what an attacker who can forge collection requests may cost the
// database: once a family's limit is reached, every further new value for that
// site and day is counted in that family's one reserved overflow row instead of
// creating another. An already-known dimension keeps its exact row.
//
// The data model states these as MVP constants rather than deployment settings.
// Making them configurable would turn a storage guarantee — at most 3,004
// aggregate rows per active site and day — into a value an operator has to get
// right, and every report and capacity estimate depends on it.
const (
	maxPathsPerSiteDay     = 2000
	maxReferrersPerSiteDay = 500
	maxCampaignsPerSiteDay = 500
)

// insertEventSQL writes the 30-day diagnostic ledger row and reports whether it
// was the first delivery.
//
// (site_id, event_id) is the idempotency key. ON CONFLICT DO NOTHING makes the
// answer exact through the statement's affected-row count: one row for the
// delivery that created the ledger entry, and none for every repeat — including
// a repeat whose payload differs, which the data model resolves in favour of
// the first accepted event.
//
// The count is read rather than a RETURNING row because the table is WITHOUT
// ROWID and there is nothing else to learn from the insert; LastInsertId is
// meaningless here for the same reason.
const insertEventSQL = `INSERT INTO page_view_events (
    site_id, event_id, received_at_ms, day, path,
    referrer_host, campaign_source, campaign_medium, campaign_name
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (site_id, event_id) DO NOTHING`

// upsertSiteStatSQL creates or increments the day's site total. The site row is
// the only aggregate with no attacker-controlled dimension, so it needs neither
// a limit nor an overflow row.
const upsertSiteStatSQL = `INSERT INTO daily_site_stats (site_id, day, page_views, visitors)
VALUES (?, ?, 1, 0)
ON CONFLICT (site_id, day) DO UPDATE SET page_views = daily_site_stats.page_views + 1
RETURNING id`

// statFamily is one aggregate family's fixed statements.
//
// The SQL is written out per family and per column rather than generated from
// table names: the data model requires four explicit families, and dynamic
// identifiers would make the statements unreviewable for the sake of removing
// repetition the schema deliberately has.
type statFamily struct {
	// stage names the family in a diagnostic. It is a constant, not a value.
	stage string
	// limit is the family's per-site/day non-overflow row limit.
	limit int
	// selectExact finds the exact dimension's row: (site_id, day, dimensions…).
	selectExact string
	// countKnown counts the day's non-overflow rows: (site_id, day).
	countKnown string
	// insertExact creates the exact dimension's row with one page view and
	// returns its id: (site_id, day, dimensions…).
	insertExact string
	// upsertOverflow creates or increments the day's one reserved overflow row
	// and returns its id: (site_id, day).
	upsertOverflow string
	// incrementViews adds one page view to a resolved row: (id).
	incrementViews string
	// insertVisitorKey records a visitor against a resolved row: (id, key).
	insertVisitorKey string
	// incrementVisitors adds one daily visitor to a resolved row: (id).
	incrementVisitors string
}

// siteFamily carries only the visitor statements: its row is resolved by
// upsertSiteStatSQL rather than by the bounded dimension resolution.
var siteFamily = statFamily{
	stage:             "site",
	insertVisitorKey:  `INSERT INTO daily_site_visitors (stat_id, visitor_key) VALUES (?, ?) ON CONFLICT DO NOTHING`,
	incrementVisitors: `UPDATE daily_site_stats SET visitors = visitors + 1 WHERE id = ?`,
}

var pageFamily = statFamily{
	stage:       "page",
	limit:       maxPathsPerSiteDay,
	selectExact: `SELECT id FROM daily_page_stats WHERE site_id = ? AND day = ? AND path = ?`,
	countKnown:  `SELECT count(*) FROM daily_page_stats WHERE site_id = ? AND day = ? AND overflow = 0`,
	insertExact: `INSERT INTO daily_page_stats (site_id, day, path, overflow, page_views, visitors)
VALUES (?, ?, ?, 0, 1, 0) RETURNING id`,
	upsertOverflow: `INSERT INTO daily_page_stats (site_id, day, path, overflow, page_views, visitors)
VALUES (?, ?, '', 1, 1, 0)
ON CONFLICT (site_id, day, path) DO UPDATE SET page_views = daily_page_stats.page_views + 1
RETURNING id`,
	incrementViews:    `UPDATE daily_page_stats SET page_views = page_views + 1 WHERE id = ?`,
	insertVisitorKey:  `INSERT INTO daily_page_visitors (stat_id, visitor_key) VALUES (?, ?) ON CONFLICT DO NOTHING`,
	incrementVisitors: `UPDATE daily_page_stats SET visitors = visitors + 1 WHERE id = ?`,
}

var referrerFamily = statFamily{
	stage:       "referrer",
	limit:       maxReferrersPerSiteDay,
	selectExact: `SELECT id FROM daily_referrer_stats WHERE site_id = ? AND day = ? AND referrer_host = ?`,
	countKnown:  `SELECT count(*) FROM daily_referrer_stats WHERE site_id = ? AND day = ? AND overflow = 0`,
	insertExact: `INSERT INTO daily_referrer_stats (site_id, day, referrer_host, overflow, page_views, visitors)
VALUES (?, ?, ?, 0, 1, 0) RETURNING id`,
	upsertOverflow: `INSERT INTO daily_referrer_stats (site_id, day, referrer_host, overflow, page_views, visitors)
VALUES (?, ?, '', 1, 1, 0)
ON CONFLICT (site_id, day, referrer_host) DO UPDATE SET page_views = daily_referrer_stats.page_views + 1
RETURNING id`,
	incrementViews:    `UPDATE daily_referrer_stats SET page_views = page_views + 1 WHERE id = ?`,
	insertVisitorKey:  `INSERT INTO daily_referrer_visitors (stat_id, visitor_key) VALUES (?, ?) ON CONFLICT DO NOTHING`,
	incrementVisitors: `UPDATE daily_referrer_stats SET visitors = visitors + 1 WHERE id = ?`,
}

var campaignFamily = statFamily{
	stage: "campaign",
	limit: maxCampaignsPerSiteDay,
	selectExact: `SELECT id FROM daily_campaign_stats
WHERE site_id = ? AND day = ? AND campaign_source = ? AND campaign_medium = ? AND campaign_name = ?`,
	countKnown: `SELECT count(*) FROM daily_campaign_stats WHERE site_id = ? AND day = ? AND overflow = 0`,
	insertExact: `INSERT INTO daily_campaign_stats
(site_id, day, campaign_source, campaign_medium, campaign_name, overflow, page_views, visitors)
VALUES (?, ?, ?, ?, ?, 0, 1, 0) RETURNING id`,
	upsertOverflow: `INSERT INTO daily_campaign_stats
(site_id, day, campaign_source, campaign_medium, campaign_name, overflow, page_views, visitors)
VALUES (?, ?, '', '', '', 1, 1, 0)
ON CONFLICT (site_id, day, campaign_source, campaign_medium, campaign_name)
DO UPDATE SET page_views = daily_campaign_stats.page_views + 1
RETURNING id`,
	incrementViews:    `UPDATE daily_campaign_stats SET page_views = page_views + 1 WHERE id = ?`,
	insertVisitorKey:  `INSERT INTO daily_campaign_visitors (stat_id, visitor_key) VALUES (?, ?) ON CONFLICT DO NOTHING`,
	incrementVisitors: `UPDATE daily_campaign_stats SET visitors = visitors + 1 WHERE id = ?`,
}

// recordPageView runs one event transaction.
//
// It is called only by the writer goroutine, which is what makes SQLite's
// single writer a property of the process and not just a convention, and its
// context is the writer's rather than the caller's: cancelling a statement
// mid-transaction interrupts SQLite and discards the pool's one connection, so
// a caller that gives up is answered by abandoning the request before it starts
// rather than by tearing down a transaction that is already running.
//
// Success is returned only after the commit succeeds. Every earlier return
// leaves the deferred rollback to undo the whole event, so a failed page view
// retains no ledger row, no aggregate increment, and no visitor key.
//
// Every statement goes through tx. The pool allows one connection, so a call on
// the database handle while this transaction is open would wait for a
// connection the transaction itself is holding, and wait forever.
func (d *Database) recordPageView(ctx context.Context, view PageView) (PageViewResult, error) {
	db, err := d.handle()
	if err != nil {
		return PageViewResult{}, err
	}
	receivedAt, day := view.day()

	// The DSN sets _txlock=immediate, so this takes the writer lock up front
	// and a transaction cannot fail on upgrade after it has already read.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return PageViewResult{}, wrapWrite("begin event transaction", err)
	}
	// Rolling back a committed transaction is a no-op; this is the path that
	// undoes a partially applied event.
	defer func() { _ = tx.Rollback() }()

	insert, err := tx.ExecContext(ctx, insertEventSQL,
		view.SiteID, view.EventID, receivedAt, day, view.Path,
		view.referrerValue(), view.CampaignSource, view.CampaignMedium, view.CampaignName,
	)
	switch {
	case isMissingSite(err):
		// The site foreign key is the authoritative answer: a site that was
		// never created, or was deleted while this event was queued, cannot
		// have a measurement filed against it. The typed error is built from
		// the code alone, so no driver text reaches the caller.
		return PageViewResult{}, describe(ErrUnknownSite, "record page view",
			"the referenced site does not exist")
	case err != nil:
		return PageViewResult{}, wrapWrite("record page view", err)
	}
	recorded, err := insert.RowsAffected()
	if err != nil {
		return PageViewResult{}, wrapWrite("record page view", err)
	}
	if recorded == 0 {
		// A duplicate delivery. Nothing is counted again and the repeated
		// payload is discarded; the transaction is still committed so that the
		// caller's success means the same thing it always does.
		if err := d.injectedFailure("commit"); err != nil {
			return PageViewResult{}, err
		}
		// As below: a failed COMMIT is rolled back by SQLite itself, so the
		// duplicate is reported as a failure rather than as a no-op that
		// happened to leave nothing behind.
		if err := tx.Commit(); err != nil {
			return PageViewResult{}, wrapWrite("commit event transaction", err)
		}
		return PageViewResult{Day: day, Duplicate: true}, nil
	}
	if err := d.injectedFailure("ledger"); err != nil {
		return PageViewResult{}, err
	}

	var siteStatID int64
	if err := tx.QueryRowContext(ctx, upsertSiteStatSQL, view.SiteID, day).Scan(&siteStatID); err != nil {
		return PageViewResult{}, wrapWrite("resolve site aggregate", err)
	}
	if err := d.injectedFailure("site"); err != nil {
		return PageViewResult{}, err
	}

	// counted pairs every resolved aggregate row with the family it belongs to,
	// in the order the data model resolves them, so the visitor pass can walk
	// exactly the rows this event touched.
	counted := make([]countedStat, 0, 4)

	pageStatID, err := resolveStat(ctx, tx, pageFamily, view.SiteID, day, view.Path)
	if err != nil {
		return PageViewResult{}, err
	}
	counted = append(counted, countedStat{family: pageFamily, id: pageStatID})
	if err := d.injectedFailure("page"); err != nil {
		return PageViewResult{}, err
	}

	if view.ReferrerHost != "" {
		referrerStatID, err := resolveStat(ctx, tx, referrerFamily, view.SiteID, day, view.ReferrerHost)
		if err != nil {
			return PageViewResult{}, err
		}
		counted = append(counted, countedStat{family: referrerFamily, id: referrerStatID})
	}
	if err := d.injectedFailure("referrer"); err != nil {
		return PageViewResult{}, err
	}

	if view.hasCampaign() {
		campaignStatID, err := resolveStat(ctx, tx, campaignFamily, view.SiteID, day,
			view.CampaignSource, view.CampaignMedium, view.CampaignName)
		if err != nil {
			return PageViewResult{}, err
		}
		counted = append(counted, countedStat{family: campaignFamily, id: campaignStatID})
	}
	if err := d.injectedFailure("campaign"); err != nil {
		return PageViewResult{}, err
	}

	if view.VisitorKey != nil {
		// The site row is always applicable; the dimension rows are the ones
		// this event resolved. A visitor that reached several overflow values
		// counts once per family because those values share one overflow row.
		if err := d.countVisitor(ctx, tx, siteFamily, siteStatID, view.VisitorKey); err != nil {
			return PageViewResult{}, err
		}
		for _, stat := range counted {
			if err := d.countVisitor(ctx, tx, stat.family, stat.id, view.VisitorKey); err != nil {
				return PageViewResult{}, err
			}
		}
	}
	if err := d.injectedFailure("visitors"); err != nil {
		return PageViewResult{}, err
	}

	if err := d.injectedFailure("commit"); err != nil {
		return PageViewResult{}, err
	}
	// A COMMIT that fails leaves nothing behind: SQLite rolls the transaction
	// back itself, and database/sql marks the transaction done either way, so
	// the deferred Rollback is a no-op rather than a second attempt. That is why
	// this error is reported like any other — the caller is told the page view
	// was not kept, and it was not. Proving it here would need a filesystem
	// fault, not a test hook; the behaviour is the driver's, and verified there.
	if err := tx.Commit(); err != nil {
		return PageViewResult{}, wrapWrite("commit event transaction", err)
	}
	return PageViewResult{Day: day}, nil
}

// countedStat is one aggregate row this event incremented.
type countedStat struct {
	family statFamily
	id     int64
}

// resolveStat finds, creates, or overflows one dimension's aggregate row and
// adds this event's page view to it.
//
// The three outcomes are the data model's bounded resolution: a dimension the
// day already knows keeps its exact row however full the family is; a new one
// gets an exact row while the family is below its limit; and a new one at the
// limit is counted in the family's single reserved overflow row, whose shape is
// overflow = 1 with the empty dimension value.
//
// Selection is therefore arrival-ordered, and the empty dimension can never
// collide with a real one: a path begins with '/', a referrer host is at least
// one byte, and a campaign tuple has at least one non-empty member.
func resolveStat(ctx context.Context, tx *sql.Tx, family statFamily, siteID, day string, dimensions ...any) (int64, error) {
	args := make([]any, 0, 2+len(dimensions))
	args = append(args, siteID, day)
	args = append(args, dimensions...)

	var id int64
	err := tx.QueryRowContext(ctx, family.selectExact, args...).Scan(&id)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, family.incrementViews, id); err != nil {
			return 0, wrapWrite("increment "+family.stage+" aggregate", err)
		}
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, wrapWrite("resolve "+family.stage+" aggregate", err)
	}

	var known int
	if err := tx.QueryRowContext(ctx, family.countKnown, siteID, day).Scan(&known); err != nil {
		return 0, wrapWrite("count "+family.stage+" dimensions", err)
	}
	statement, statementArgs := family.insertExact, args
	if known >= family.limit {
		statement, statementArgs = family.upsertOverflow, args[:2]
	}
	if err := tx.QueryRowContext(ctx, statement, statementArgs...).Scan(&id); err != nil {
		return 0, wrapWrite("create "+family.stage+" aggregate", err)
	}
	return id, nil
}

// countVisitor records the visitor against one aggregate row and increments its
// daily visitor count only when the deduplication row was actually new.
//
// The composite primary key (stat_id, visitor_key) is what makes "already
// counted in this exact site, day, and dimension" a question SQLite answers,
// rather than one the application has to keep state for.
//
// The deduplication row and the count it justifies are two statements, and they
// have to stand or fall together: a row without its increment would silently
// lose a visitor for the rest of the day, because the next delivery would find
// the key already present and count nothing. The transaction is what guarantees
// that, and the injection point between them is what proves it.
func (d *Database) countVisitor(ctx context.Context, tx *sql.Tx, family statFamily, statID int64, key []byte) error {
	result, err := tx.ExecContext(ctx, family.insertVisitorKey, statID, key)
	if err != nil {
		return wrapWrite("record "+family.stage+" visitor", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return wrapWrite("record "+family.stage+" visitor", err)
	}
	if inserted == 0 {
		return nil
	}
	if err := d.injectedFailure(family.stage + " visitor increment"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, family.incrementVisitors, statID); err != nil {
		return wrapWrite("increment "+family.stage+" visitors", err)
	}
	return nil
}
