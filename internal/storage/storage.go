// Package storage owns the PacaPulse SQLite database lifecycle.
//
// It creates the data directory and database with owner-only permissions, holds
// an exclusive advisory lock for as long as the process lives, applies the
// embedded forward-only migrations, and verifies that SQLite is really running
// with the settings docs/architecture/data-model.md requires — rather than
// assuming that a PRAGMA statement which returned no error took effect.
//
// The database location is not configurable beyond its directory: the file
// names beneath it belong to the application and no SQLite DSN is accepted from
// the environment, so a deployment cannot point the service at shared or
// unexpected storage.
//
// Failures are reported without filesystem paths, SQL values, or database
// content, so that they can be logged and surfaced through readiness without
// disclosing where a deployment keeps its data.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// The CGO-free SQLite driver. It is a pure-Go translation of SQLite, which
	// is what keeps the production binary statically linked and the runtime
	// image distroless.
	_ "modernc.org/sqlite"
)

// Fixed application-owned names beneath the configured data directory.
const (
	// DatabaseFileName is the analytics database.
	DatabaseFileName = "pacapulse.db"
	// LockFileName is the advisory lock every writing entry point must hold.
	LockFileName = "pacapulse.lock"
)

// driverName is the driver modernc.org/sqlite registers.
const driverName = "sqlite"

// Permissions for what the service creates. The volume holds analytics data and
// short-lived visitor keys, so nothing it creates is group- or world-readable.
// SQLite copies the database file's mode onto the -wal and -shm files, which is
// what keeps the write-ahead log unreadable too.
const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// Options configures Open.
type Options struct {
	// Dir is the configured data directory. It is created when absent.
	Dir string
	// Now supplies the timestamp recorded with an applied migration. Nil
	// selects time.Now. Migration time is assigned by the application rather
	// than by SQLite's ambient clock.
	Now func() time.Time
}

// Database is the service's SQLite database and the lock that makes this
// process its only writer.
type Database struct {
	lock *lockFile
	now  func() time.Time

	mu      sync.Mutex
	db      *sql.DB
	version int
	// initErr is the sticky reason the database is unusable. Nothing clears it:
	// the service does not attempt automatic repair, and an operator restores
	// or repairs the volume deliberately.
	initErr error
	closed  bool
}

// Open prepares the data directory, takes the exclusive lock, and opens the
// database handle.
//
// Its errors are the two the process cannot start with: an unusable data
// directory (ErrDataDirectory) and a lock another process owns (ErrLocked). The
// database is not yet checked or migrated — call Initialize for that, whose
// failures are survivable.
func Open(opts Options) (*Database, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return nil, describe(ErrDataDirectory, "resolve data directory", "no data directory is configured")
	}
	// SQLite is handed an absolute path so that the database a running service
	// owns cannot change meaning with the working directory.
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, wrap(ErrDataDirectory, "resolve data directory", err)
	}
	// MkdirAll leaves an existing directory's permissions alone — a mounted
	// volume's ownership is the platform's to decide — and reports the failure
	// when the path exists as something other than a directory.
	if err := os.MkdirAll(absolute, dirMode); err != nil {
		return nil, wrap(ErrDataDirectory, "create data directory", err)
	}

	// The lock is taken before the database is touched, so a second process
	// cannot even create the file, and it is the first thing to fail on a
	// read-only volume.
	lock, err := acquireLock(filepath.Join(absolute, LockFileName))
	if err != nil {
		return nil, err
	}

	path := filepath.Join(absolute, DatabaseFileName)
	if err := createDatabaseFile(path); err != nil {
		_ = lock.release()
		return nil, err
	}

	db, err := sql.Open(driverName, dataSourceName(path))
	if err != nil {
		_ = lock.release()
		return nil, wrap(ErrDataDirectory, "open database", err)
	}
	// One serialized writer, as the data model requires. It also makes the
	// verified connection settings a property of the connection actually in
	// use, and keeps the migration runner from racing itself.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Database{
		lock:    lock,
		now:     now,
		db:      db,
		initErr: ErrUninitialized,
	}, nil
}

// createDatabaseFile creates an empty database with owner-only permissions.
//
// SQLite would otherwise create it under the process umask. An empty file is a
// valid empty database, so creating it here costs nothing and decides the mode
// the -wal and -shm files inherit. An existing file's permissions are left
// alone: they belong to whoever provisioned the volume.
func createDatabaseFile(path string) error {
	// #nosec G304 -- the path is application-owned: a fixed file name beneath
	// the configured data directory.
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, fileMode)
	switch {
	case errors.Is(err, fs.ErrExist):
		return nil
	case err != nil:
		return wrap(ErrDataDirectory, "create database file", err)
	}
	if err := file.Close(); err != nil {
		return wrap(ErrDataDirectory, "create database file", err)
	}
	return nil
}

// Initialize checks the database, applies pending migrations, and verifies the
// result.
//
// A failure here is deliberately survivable: corrupt content, an unexpected
// configuration, a checksum mismatch, a newer schema, a failed migration, or a
// failed post-migration check leaves the process alive for diagnosis while
// readiness reports the database unavailable. The reason is remembered, so a
// later probe reports the same failure instead of retrying a repair the service
// has no business attempting.
func (d *Database) Initialize(ctx context.Context) error {
	version, err := d.initialize(ctx)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.initErr = err
	d.version = version
	return err
}

func (d *Database) initialize(ctx context.Context) (int, error) {
	db, err := d.handle()
	if err != nil {
		return 0, err
	}

	// The integrity gate comes first: journal_mode is a persistent conversion,
	// and nothing should be written to a file that may not be a database, or
	// may be a damaged one an operator still wants to investigate.
	if err := quickCheck(ctx, db); err != nil {
		return 0, err
	}
	// Then the settings, while the file may still have no tables — which is the
	// only moment page_size and auto_vacuum can be chosen.
	if err := configure(ctx, db); err != nil {
		return 0, err
	}
	if err := verifyPersistentPragmas(ctx, db); err != nil {
		return 0, err
	}
	if err := verifyConnectionPragmas(ctx, db); err != nil {
		return 0, err
	}

	version, err := d.migrate(ctx, db, embeddedMigrations)
	if err != nil {
		return 0, err
	}

	// And afterwards, prove that what the migrations produced is the database
	// the data model describes.
	if err := verifyPersistentPragmas(ctx, db); err != nil {
		return 0, err
	}
	if err := verifyConnectionPragmas(ctx, db); err != nil {
		return 0, err
	}
	if err := foreignKeyCheck(ctx, db); err != nil {
		return 0, err
	}
	if err := orphanCheck(ctx, db); err != nil {
		return 0, err
	}
	return version, nil
}

// Check reports whether the database is usable. It is registered as the
// readiness dependency, so it must stay cheap: the expensive integrity check
// belongs to startup, while this confirms that the connection in use still
// enforces the contract and still holds the schema this binary migrated.
func (d *Database) Check(ctx context.Context) error {
	d.mu.Lock()
	db, initErr, version, closed := d.db, d.initErr, d.version, d.closed
	d.mu.Unlock()

	switch {
	case closed:
		return ErrClosed
	case initErr != nil:
		return initErr
	}

	if err := db.PingContext(ctx); err != nil {
		return wrap(classify(err), "ping database", err)
	}
	if err := verifyPersistentPragmas(ctx, db); err != nil {
		return err
	}
	if err := verifyConnectionPragmas(ctx, db); err != nil {
		return err
	}
	current, err := recordedVersion(ctx, db)
	if err != nil {
		return err
	}
	if current != version {
		return describe(ErrConfiguration, "verify schema version",
			fmt.Sprintf("database reports version %d, this process migrated to %d", current, version))
	}
	return nil
}

// Version returns the schema version the database was migrated to, or zero when
// initialization has not succeeded.
func (d *Database) Version() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.initErr != nil {
		return 0
	}
	return d.version
}

// Close releases the database and the lock. It is safe to call more than once,
// and it is what a failed startup calls too: the lock must not outlive the
// process that took it, or the next start would refuse itself.
func (d *Database) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	db, lock := d.db, d.lock
	d.db, d.lock = nil, nil
	d.mu.Unlock()

	var closeErr error
	if db != nil {
		if err := db.Close(); err != nil {
			// Shutdown is not one of the classified states: there is nothing
			// left to be ready or unready about, only a fact to report.
			closeErr = fmt.Errorf("close database: %s", sanitize(err))
		}
	}
	// The lock is released even when closing the database failed; leaving it
	// held would block the restart that failure calls for.
	if err := lock.release(); err != nil && closeErr == nil {
		closeErr = err
	}
	return closeErr
}

// handle returns the live database handle.
func (d *Database) handle() (*sql.DB, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.db == nil {
		return nil, ErrClosed
	}
	return d.db, nil
}
