package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
)

// Settings SQLite stores in the database file itself.
//
// page_size and auto_vacuum are only honoured while the file still has no
// tables, and changing either afterwards needs a full VACUUM and roughly a
// second database's worth of free space. The 8 KiB page is what keeps the
// largest bounded event record inside an index B-tree's local payload; see
// docs/architecture/data-model.md.
const (
	requiredPageSize = 8192
	// autoVacuumIncremental is the value PRAGMA auto_vacuum reports for
	// INCREMENTAL, which lets maintenance return free pages in bounded steps
	// instead of taking the service down for a full VACUUM.
	autoVacuumIncremental = 2
	// requiredJournalMode is reported in lower case by SQLite.
	requiredJournalMode = "wal"
)

// Settings scoped to a connection rather than to the file. SQLite applies these
// per connection, so they are carried by the data source name — which the
// driver replays on every connection it opens — and then verified rather than
// assumed.
const (
	// busyTimeoutMillis bounds how long a statement waits for the writer.
	busyTimeoutMillis = 5000
	// journalSizeLimitBytes caps the WAL retained after a checkpoint at 64 MiB.
	journalSizeLimitBytes = 64 << 20
	// walAutocheckpointPages checkpoints the WAL every 1000 pages.
	walAutocheckpointPages = 1000
	// synchronousFull is the value PRAGMA synchronous reports for FULL, which
	// is what makes a committed analytics event survive a power failure.
	synchronousFull = 2
)

// pragma is one setting and the value SQLite must report for it. Values are
// compared as text because SQLite answers some pragmas with a number and others
// with a keyword.
type pragma struct {
	name string
	want string
}

func (p pragma) statement() string { return "PRAGMA " + p.name }

// persistentPragmas are properties of the database file. A database that does
// not report them was created by something else, or under a different
// configuration, and is refused rather than silently used.
var persistentPragmas = []pragma{
	{name: "page_size", want: fmt.Sprint(requiredPageSize)},
	{name: "auto_vacuum", want: fmt.Sprint(autoVacuumIncremental)},
	{name: "journal_mode", want: requiredJournalMode},
}

// connectionPragmas are properties of a connection. Relying on the connection
// that ran the migrations is not enough: the data model requires every
// connection — reads, maintenance, backup, and repair tooling alike — to
// enforce foreign keys and secure deletion.
var connectionPragmas = []pragma{
	{name: "busy_timeout", want: fmt.Sprint(busyTimeoutMillis)},
	{name: "foreign_keys", want: "1"},
	{name: "journal_size_limit", want: fmt.Sprint(journalSizeLimitBytes)},
	{name: "secure_delete", want: "1"},
	{name: "synchronous", want: fmt.Sprint(synchronousFull)},
	{name: "wal_autocheckpoint", want: fmt.Sprint(walAutocheckpointPages)},
}

// dataSourceName builds the driver DSN for an absolute database path.
//
// The connection settings travel in the DSN so that any connection the pool
// opens — not just the first — starts out configured. The path is URI-escaped
// because SQLite parses the whole string as a URI.
func dataSourceName(path string) string {
	params := url.Values{}
	for _, p := range connectionPragmas {
		params.Add("_pragma", p.name+"("+p.want+")")
	}
	// Every write transaction takes the writer lock up front, so a transaction
	// cannot fail on upgrade after it has already read.
	params.Set("_txlock", "immediate")

	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + params.Encode()
}

// configure applies the settings SQLite stores in the file. On a fresh database
// this is the only moment page_size and auto_vacuum can be chosen; on an
// existing one SQLite ignores them silently, which is exactly why the values
// are verified afterwards instead of being assumed to have taken effect.
func configure(ctx context.Context, db *sql.DB) error {
	statements := []string{
		fmt.Sprintf("PRAGMA page_size = %d", requiredPageSize),
		"PRAGMA auto_vacuum = INCREMENTAL",
		"PRAGMA journal_mode = WAL",
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return wrap(classify(err), "configure database", err)
		}
	}
	return nil
}

// verifyPragmas asks SQLite what it is actually doing and rejects any answer
// that differs from the contract.
func verifyPragmas(ctx context.Context, db *sql.DB, stage string, required []pragma) error {
	for _, p := range required {
		var got string
		if err := db.QueryRowContext(ctx, p.statement()).Scan(&got); err != nil {
			return wrap(classify(err), stage, err)
		}
		if got != p.want {
			return describe(ErrConfiguration, stage,
				fmt.Sprintf("%s is %q, want %q", p.name, got, p.want))
		}
	}
	return nil
}

func verifyPersistentPragmas(ctx context.Context, db *sql.DB) error {
	return verifyPragmas(ctx, db, "verify database settings", persistentPragmas)
}

func verifyConnectionPragmas(ctx context.Context, db *sql.DB) error {
	return verifyPragmas(ctx, db, "verify connection settings", connectionPragmas)
}
