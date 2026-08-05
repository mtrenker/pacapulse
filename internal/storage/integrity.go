package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	sqlite "modernc.org/sqlite"
)

// SQLite primary result codes this package classifies on. They are part of
// SQLite's stable public interface.
const (
	resultCorrupt = 11 // SQLITE_CORRUPT
	resultNotADB  = 26 // SQLITE_NOTADB
)

// classify maps a driver failure onto the class that describes it. A file that
// is not a database, or one whose pages are malformed, is an integrity failure
// however innocuous the statement that discovered it was.
func classify(err error) error {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		// The extended result code carries the primary code in its low byte.
		switch sqliteErr.Code() & 0xff {
		case resultCorrupt, resultNotADB:
			return ErrIntegrity
		}
	}
	return ErrConfiguration
}

// quickCheck runs the startup integrity check. It is quick_check rather than
// integrity_check because startup must not read the whole database before it
// can serve; the full check belongs to the restore procedure, where the cost is
// paid once against a candidate backup.
func quickCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return wrap(classify(err), "integrity check", err)
	}
	defer func() { _ = rows.Close() }()

	var results []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return wrap(classify(err), "integrity check", err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return wrap(classify(err), "integrity check", err)
	}
	if len(results) != 1 || results[0] != "ok" {
		// The findings name pages and b-trees of a database this service is
		// refusing to serve; the count is what an operator acts on.
		return describe(ErrIntegrity, "integrity check",
			fmt.Sprintf("quick_check reported %d problems", len(results)))
	}
	return nil
}

// foreignKeyCheck rejects a database whose declared references are not
// satisfied. It returns one row per violation and no rows for a sound database.
func foreignKeyCheck(ctx context.Context, db *sql.DB) error {
	var violations int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil {
		return wrap(classify(err), "foreign key check", err)
	}
	if violations > 0 {
		return describe(ErrIntegrity, "foreign key check",
			fmt.Sprintf("%d foreign key violations", violations))
	}
	return nil
}

// orphanChecks are the four explicit aggregate/visitor joins the data model
// requires at startup and after every retention pass.
//
// They are not redundant with foreign_key_check. A visitor row survives only as
// long as its aggregate row, and AUTOINCREMENT exists so that a stale reference
// can never be made valid again by rowid reuse; these joins are what proves
// both, including in a database whose foreign keys were disabled during an
// older table rebuild.
// The queries are written out rather than generated from table names so that
// each pair is reviewable as SQL, the way the data model states them.
var orphanChecks = []struct {
	visitors string
	query    string
}{
	{
		visitors: "daily_site_visitors",
		query: "SELECT count(*) FROM daily_site_visitors AS v " +
			"LEFT JOIN daily_site_stats AS s ON s.id = v.stat_id WHERE s.id IS NULL",
	},
	{
		visitors: "daily_page_visitors",
		query: "SELECT count(*) FROM daily_page_visitors AS v " +
			"LEFT JOIN daily_page_stats AS s ON s.id = v.stat_id WHERE s.id IS NULL",
	},
	{
		visitors: "daily_referrer_visitors",
		query: "SELECT count(*) FROM daily_referrer_visitors AS v " +
			"LEFT JOIN daily_referrer_stats AS s ON s.id = v.stat_id WHERE s.id IS NULL",
	},
	{
		visitors: "daily_campaign_visitors",
		query: "SELECT count(*) FROM daily_campaign_visitors AS v " +
			"LEFT JOIN daily_campaign_stats AS s ON s.id = v.stat_id WHERE s.id IS NULL",
	},
}

// orphanCheck reports visitor rows whose aggregate row is missing.
func orphanCheck(ctx context.Context, db *sql.DB) error {
	for _, check := range orphanChecks {
		var orphans int
		if err := db.QueryRowContext(ctx, check.query).Scan(&orphans); err != nil {
			return wrap(classify(err), "orphan check", err)
		}
		if orphans > 0 {
			return describe(ErrOrphaned, "orphan check",
				fmt.Sprintf("%d rows in %s have no aggregate row", orphans, check.visitors))
		}
	}
	return nil
}
