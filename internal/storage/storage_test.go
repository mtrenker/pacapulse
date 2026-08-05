// The storage tests are in the package rather than beside it: several of them
// have to drive the migration runner with a deliberately broken migration set,
// or reach the database behind the lifecycle to plant the corruption a
// production database could arrive with.
package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedTime is the migration clock used by the tests, so that a recorded
// timestamp can be asserted exactly rather than approximately.
var fixedTime = time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)

// open opens and initializes a database in dir, failing the test if the
// database does not come up.
func open(t *testing.T, dir string) *Database {
	t.Helper()

	db := openOnly(t, dir)
	if err := db.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v, want nil", err)
	}
	return db
}

// openOnly opens without initializing, for the tests that want to inspect or
// break what Initialize would find.
func openOnly(t *testing.T, dir string) *Database {
	t.Helper()

	db, err := Open(Options{Dir: dir, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// query runs a single-value query against the live database.
func query[T any](t *testing.T, db *Database, statement string, args ...any) T {
	t.Helper()

	handle, err := db.handle()
	if err != nil {
		t.Fatalf("handle() error = %v", err)
	}
	var value T
	if err := handle.QueryRowContext(context.Background(), statement, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	return value
}

// exec runs a statement against the live database.
func exec(t *testing.T, db *Database, statement string, args ...any) {
	t.Helper()

	handle, err := db.handle()
	if err != nil {
		t.Fatalf("handle() error = %v", err)
	}
	if _, err := handle.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// TestFreshDatabaseMatchesTheContract covers what a first start has to produce:
// the accepted schema, the settings SQLite must actually be running with, and a
// migration record an operator can compare against the binary.
func TestFreshDatabaseMatchesTheContract(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	db := open(t, dir)

	if got := db.Version(); got != len(embeddedMigrations) {
		t.Errorf("Version() = %d, want %d", got, len(embeddedMigrations))
	}
	if err := db.Check(context.Background()); err != nil {
		t.Errorf("Check() error = %v, want nil", err)
	}

	for _, p := range append(append([]pragma{}, persistentPragmas...), connectionPragmas...) {
		if got := query[string](t, db, p.statement()); got != p.want {
			t.Errorf("%s = %q, want %q", p.name, got, p.want)
		}
	}

	// Migration 1's objects, spot-checked across the four table families the
	// data model names, plus the retention indexes reports and cleanup need.
	for _, name := range []string{
		"organizations", "sites", "site_hosts", "page_view_events",
		"daily_site_stats", "daily_page_stats", "daily_referrer_stats", "daily_campaign_stats",
		"daily_site_visitors", "daily_page_visitors", "daily_referrer_visitors", "daily_campaign_visitors",
		"site_deletion_audit", "schema_migrations",
	} {
		got := query[int](t, db, fmt.Sprintf(
			"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = '%s'", name))
		if got != 1 {
			t.Errorf("table %s is missing", name)
		}
	}
	for _, name := range []string{
		"page_view_events_retention_idx", "page_view_events_site_day_idx",
		"daily_site_stats_retention_idx", "daily_page_stats_retention_idx",
		"daily_referrer_stats_retention_idx", "daily_campaign_stats_retention_idx",
		"site_deletion_audit_deleted_at_idx",
	} {
		got := query[int](t, db, fmt.Sprintf(
			"SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = '%s'", name))
		if got != 1 {
			t.Errorf("index %s is missing", name)
		}
	}

	var (
		version   int
		checksum  string
		appliedAt int64
	)
	handle, err := db.handle()
	if err != nil {
		t.Fatalf("handle() error = %v", err)
	}
	row := handle.QueryRow("SELECT version, checksum, applied_at_ms FROM schema_migrations")
	if err := row.Scan(&version, &checksum, &appliedAt); err != nil {
		t.Fatalf("read migration record: %v", err)
	}
	if version != 1 {
		t.Errorf("recorded version = %d, want 1", version)
	}
	// The checksum is of the embedded file's exact bytes, recomputed here
	// rather than taken from the runner that wrote it.
	body, err := migrationDir.ReadFile("migrations/" + embeddedMigrations[0].name)
	if err != nil {
		t.Fatalf("read embedded migration: %v", err)
	}
	sum := sha256.Sum256(body)
	if want := hex.EncodeToString(sum[:]); checksum != want {
		t.Errorf("recorded checksum = %q, want %q", checksum, want)
	}
	if want := fixedTime.UnixMilli(); appliedAt != want {
		t.Errorf("recorded applied_at_ms = %d, want the application clock's %d", appliedAt, want)
	}
}

// TestCreatedFilesAreOwnerOnly proves the volume does not become readable to
// other accounts, including through the write-ahead log.
func TestCreatedFilesAreOwnerOnly(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	db := open(t, dir)
	// A checkpointed WAL can be empty; a write guarantees there is one.
	exec(t, db, "INSERT INTO organizations (id, name, created_at_ms) VALUES "+
		"('11111111-1111-4111-8111-111111111111', 'Example', 0)")

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat data directory: %v", err)
	}
	if got := info.Mode().Perm(); got != dirMode {
		t.Errorf("data directory mode = %v, want %v", got, dirMode)
	}

	for _, name := range []string{DatabaseFileName, LockFileName, DatabaseFileName + "-wal", DatabaseFileName + "-shm"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got&0o077 != 0 {
			t.Errorf("%s mode = %v, want no group or world access", name, got)
		}
	}
}

// TestRestartIsIdempotent covers the ordinary case that happens on every
// deployment: the second start must change nothing.
func TestRestartIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	first := open(t, dir)
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	later := fixedTime.Add(72 * time.Hour)
	second, err := Open(Options{Dir: dir, Now: func() time.Time { return later }})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := second.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v, want nil", err)
	}

	if got := query[int](t, second, "SELECT count(*) FROM schema_migrations"); got != len(embeddedMigrations) {
		t.Errorf("migration records = %d, want %d", got, len(embeddedMigrations))
	}
	got := query[int64](t, second, "SELECT applied_at_ms FROM schema_migrations WHERE version = 1")
	if want := fixedTime.UnixMilli(); got != want {
		t.Errorf("applied_at_ms = %d after a restart, want the original %d", got, want)
	}
}

// TestChecksumMismatchIsNotServed covers an applied migration whose SQL has
// changed: the database may be perfectly readable and still not be the schema
// this binary was built against.
func TestChecksumMismatchIsNotServed(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	first := open(t, dir)
	exec(t, first, "UPDATE schema_migrations SET checksum = "+
		"'0000000000000000000000000000000000000000000000000000000000000000' WHERE version = 1")
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	second := openOnly(t, dir)
	err := second.Initialize(context.Background())
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("Initialize() error = %v, want %v", err, ErrChecksum)
	}
	if !errors.Is(second.Check(context.Background()), ErrChecksum) {
		t.Error("Check() should keep reporting the mismatch")
	}
	if got := second.Version(); got != 0 {
		t.Errorf("Version() = %d, want 0 for a database that is not served", got)
	}
	// Nothing was re-applied over the top of the mismatch.
	if got := query[int](t, second, "SELECT count(*) FROM schema_migrations"); got != 1 {
		t.Errorf("migration records = %d, want the untouched 1", got)
	}
}

// TestNewerSchemaIsRejected covers a rollback to an older binary against a
// database a newer one has already migrated.
func TestNewerSchemaIsRejected(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	first := open(t, dir)
	exec(t, first, "INSERT INTO schema_migrations (version, checksum, applied_at_ms) VALUES "+
		"(99, '1111111111111111111111111111111111111111111111111111111111111111', 0)")
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	second := openOnly(t, dir)
	err := second.Initialize(context.Background())
	if !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("Initialize() error = %v, want %v", err, ErrSchemaNewer)
	}
	if !errors.Is(second.Check(context.Background()), ErrSchemaNewer) {
		t.Error("Check() should keep reporting the newer schema")
	}
}

// TestNonDatabaseContentIsNotFatal is the corrupt-volume case: the process must
// stay up and diagnosable, and say so without describing the file.
func TestNonDatabaseContentIsNotFatal(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, DatabaseFileName), []byte("not a database at all"), fileMode); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	db, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open() error = %v, want a survivable failure instead", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	initErr := db.Initialize(context.Background())
	if !errors.Is(initErr, ErrIntegrity) {
		t.Fatalf("Initialize() error = %v, want %v", initErr, ErrIntegrity)
	}
	assertPathFree(t, initErr, dir)
	if err := db.Check(context.Background()); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Check() error = %v, want %v", err, ErrIntegrity)
	}
}

// TestCorruptPagesAreDetected covers a database that opens but whose content
// has been damaged underneath it.
func TestCorruptPagesAreDetected(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	first := open(t, dir)
	// Rows give the b-trees something to be inconsistent about, and the
	// checkpoint moves them into the main database file.
	exec(t, first, "INSERT INTO organizations (id, name, created_at_ms) VALUES "+
		"('11111111-1111-4111-8111-111111111111', 'Example', 0)")
	exec(t, first, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	path := filepath.Join(dir, DatabaseFileName)
	file, err := os.OpenFile(path, os.O_RDWR, fileMode)
	if err != nil {
		t.Fatalf("open database file: %v", err)
	}
	// Overwrite a whole interior page, leaving the header intact so the file is
	// still recognisably a database.
	if _, err := file.WriteAt([]byte(strings.Repeat("\xa5", requiredPageSize)), requiredPageSize); err != nil {
		t.Fatalf("corrupt database file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close database file: %v", err)
	}

	second := openOnly(t, dir)
	initErr := second.Initialize(context.Background())
	if !errors.Is(initErr, ErrIntegrity) {
		t.Fatalf("Initialize() error = %v, want %v", initErr, ErrIntegrity)
	}
	assertPathFree(t, initErr, dir)
}

// TestUnexpectedPageSizeIsRejected covers a database created elsewhere, or by
// an earlier configuration: page_size and auto_vacuum cannot be changed without
// a full VACUUM, so serving it would silently accept the wrong storage layout.
func TestUnexpectedPageSizeIsRejected(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	path := filepath.Join(dir, DatabaseFileName)

	foreign, err := sql.Open(driverName, "file:"+path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	for _, statement := range []string{
		"PRAGMA page_size = 4096",
		"PRAGMA journal_mode = WAL",
		"CREATE TABLE unrelated (id INTEGER PRIMARY KEY)",
	} {
		if _, err := foreign.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := foreign.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	db := openOnly(t, dir)
	initErr := db.Initialize(context.Background())
	if !errors.Is(initErr, ErrConfiguration) {
		t.Fatalf("Initialize() error = %v, want %v", initErr, ErrConfiguration)
	}
	assertPathFree(t, initErr, dir)
	if got := query[int](t, db, "SELECT count(*) FROM sqlite_master WHERE name = 'schema_migrations'"); got != 0 {
		t.Error("the migration ledger was created in a database that should not have been migrated")
	}
}

// TestOrphanedVisitorRowsAreDetected covers the check foreign keys alone cannot
// be trusted for: a visitor key whose aggregate row is gone.
func TestOrphanedVisitorRowsAreDetected(t *testing.T) {
	t.Parallel()

	for _, check := range orphanChecks {
		t.Run(check.visitors, func(t *testing.T) {
			t.Parallel()

			dir := filepath.Join(t.TempDir(), "data")
			first := open(t, dir)

			// Foreign keys are what normally makes this impossible; an older
			// database rebuilt with them disabled is exactly the case the
			// explicit checks exist for.
			exec(t, first, "PRAGMA foreign_keys = OFF")
			exec(t, first, fmt.Sprintf(
				"INSERT INTO %s (stat_id, visitor_key) VALUES (4242, randomblob(32))", check.visitors))
			exec(t, first, "PRAGMA foreign_keys = ON")

			handle, err := first.handle()
			if err != nil {
				t.Fatalf("handle() error = %v", err)
			}
			ctx := context.Background()
			if err := orphanCheck(ctx, handle); !errors.Is(err, ErrOrphaned) {
				t.Errorf("orphanCheck() error = %v, want %v", err, ErrOrphaned)
			}
			if err := foreignKeyCheck(ctx, handle); !errors.Is(err, ErrIntegrity) {
				t.Errorf("foreignKeyCheck() error = %v, want %v", err, ErrIntegrity)
			}
			if err := first.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			// And a restart against that database refuses to serve it.
			second := openOnly(t, dir)
			if err := second.Initialize(ctx); !errors.Is(err, ErrIntegrity) && !errors.Is(err, ErrOrphaned) {
				t.Errorf("Initialize() error = %v, want an integrity or orphan failure", err)
			}
		})
	}
}

// TestLockContentionRejectsASecondWriter is the guarantee ReadWriteOnce does not
// give: no second process may open the same database for writing.
func TestLockContentionRejectsASecondWriter(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	first := open(t, dir)

	// flock is held by the open file description, so a second acquisition
	// contends exactly as another process would.
	second, err := Open(Options{Dir: dir})
	if !errors.Is(err, ErrLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("Open() error = %v, want %v", err, ErrLocked)
	}
	if second != nil {
		t.Fatal("Open() returned a database while another process held the lock")
	}
	assertPathFree(t, err, dir)

	// Releasing it hands the volume to the next process.
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	third, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open() after Close() error = %v, want nil", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestFailedInitializationReleasesTheLock proves a database the service refuses
// to serve does not also strand the volume.
func TestFailedInitializationReleasesTheLock(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatalf("create data directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, DatabaseFileName), []byte("not a database"), fileMode); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	db, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Initialize(context.Background()); err == nil {
		t.Fatal("Initialize() error = nil, want a failure")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// Closing twice is what a failed startup and a shutdown path together do.
	if err := db.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want nil", err)
	}
	if err := db.Check(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Check() error = %v, want %v", err, ErrClosed)
	}

	next, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open() after a failed start error = %v, want the lock to be free", err)
	}
	if err := next.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestUnusableDataDirectoryFailsStartup covers the failures that must stop the
// process instead of degrading it: there is nowhere to put the database.
func TestUnusableDataDirectoryFailsStartup(t *testing.T) {
	t.Parallel()

	t.Run("path is a file", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "data")
		if err := os.WriteFile(path, nil, fileMode); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		_, err := Open(Options{Dir: path})
		if !errors.Is(err, ErrDataDirectory) {
			t.Fatalf("Open() error = %v, want %v", err, ErrDataDirectory)
		}
		assertPathFree(t, err, path)
	})

	t.Run("directory is not writable", func(t *testing.T) {
		t.Parallel()

		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		parent := t.TempDir()
		dir := filepath.Join(parent, "data")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatalf("create fixture: %v", err)
		}
		_, err := Open(Options{Dir: dir})
		if !errors.Is(err, ErrDataDirectory) {
			t.Fatalf("Open() error = %v, want %v", err, ErrDataDirectory)
		}
		assertPathFree(t, err, dir)
	})

	t.Run("no directory configured", func(t *testing.T) {
		t.Parallel()

		if _, err := Open(Options{Dir: "  "}); !errors.Is(err, ErrDataDirectory) {
			t.Fatalf("Open() error = %v, want %v", err, ErrDataDirectory)
		}
	})
}

// TestUninitializedDatabaseIsNotReady keeps readiness false in the window
// between opening the file and finishing the migrations.
func TestUninitializedDatabaseIsNotReady(t *testing.T) {
	t.Parallel()

	db := openOnly(t, filepath.Join(t.TempDir(), "data"))
	if err := db.Check(context.Background()); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Check() error = %v, want %v", err, ErrUninitialized)
	}
}

// TestCheckRejectsADisabledConnection proves readiness is a question asked of
// SQLite rather than an assumption carried over from startup.
func TestCheckRejectsADisabledConnection(t *testing.T) {
	t.Parallel()

	db := open(t, filepath.Join(t.TempDir(), "data"))
	exec(t, db, "PRAGMA foreign_keys = OFF")

	err := db.Check(context.Background())
	if !errors.Is(err, ErrConfiguration) {
		t.Fatalf("Check() error = %v, want %v", err, ErrConfiguration)
	}
	// The diagnostic reports what SQLite answered, which is what makes it
	// actionable without an operator having to reproduce the query.
	if !strings.Contains(err.Error(), `foreign_keys is "0"`) {
		t.Errorf("Check() error = %v, want it to name the setting and its effective value", err)
	}
}

// assertPathFree fails when a diagnostic carries the filesystem path it was
// produced from. Storage errors reach the operator log and readiness
// diagnostics, and neither may disclose where a deployment keeps its data.
func assertPathFree(t *testing.T, err error, dir string) {
	t.Helper()

	if err == nil {
		t.Fatal("expected an error to inspect")
	}
	message := err.Error()
	for _, forbidden := range []string{dir, filepath.Dir(dir), DatabaseFileName, LockFileName} {
		if forbidden == "" || forbidden == "." {
			continue
		}
		if strings.Contains(message, forbidden) {
			t.Errorf("error message contains %q: %s", forbidden, message)
		}
	}
}
