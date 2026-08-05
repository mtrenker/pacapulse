package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Failure classes.
//
// The split matters operationally. ErrDataDirectory and ErrLocked mean the
// process must not run at all: without a writable directory it has nowhere to
// put the database, and without the exclusive lock it would become a second
// writer against a store that allows exactly one. Every other class describes a
// database this binary refuses to serve from — the process stays alive and
// diagnosable while readiness reports the failure, and nothing is repaired
// automatically.
var (
	// ErrDataDirectory reports a data directory that cannot be created or
	// cannot hold the lock and database files.
	ErrDataDirectory = errors.New("data directory is unusable")
	// ErrLocked reports that another process already owns the exclusive
	// database lock.
	ErrLocked = errors.New("another process owns the database lock")

	// ErrIntegrity reports a failed integrity check or content that is not a
	// SQLite database at all.
	ErrIntegrity = errors.New("database failed its integrity check")
	// ErrConfiguration reports effective SQLite settings that differ from the
	// ones the data model requires.
	ErrConfiguration = errors.New("database configuration does not match the schema contract")
	// ErrChecksum reports an applied migration that differs from the embedded
	// migration of the same version.
	ErrChecksum = errors.New("applied migration does not match the embedded migration")
	// ErrSchemaNewer reports a database written by a newer binary.
	ErrSchemaNewer = errors.New("database schema is newer than this binary")
	// ErrMigration reports a migration that could not be applied. The previous
	// committed schema is left intact.
	ErrMigration = errors.New("migration failed")
	// ErrOrphaned reports visitor rows whose aggregate row is missing.
	ErrOrphaned = errors.New("visitor rows reference missing aggregate rows")

	// ErrClosed reports use of a database that has been shut down.
	ErrClosed = errors.New("database is closed")
	// ErrUninitialized reports a database that has been opened but not yet
	// checked and migrated. It keeps readiness false during startup.
	ErrUninitialized = errors.New("database is not initialized yet")
)

// Ingestion failures.
//
// These describe one offered page view rather than the database as a whole, so
// none of them makes the service unready. ErrQueueFull and ErrUnavailable are
// the retryable pair: the work was refused, not lost, and nothing was written.
//
// Like every other diagnostic in this package they name the field, the stage,
// or the condition — never a site or event identifier, a path, a referrer, a
// campaign value, a visitor key, or the SQL that discovered the problem.
var (
	// ErrInvalidPageView reports a page view that does not satisfy the
	// ingestion contract. It is rejected before a transaction is opened.
	ErrInvalidPageView = errors.New("page view does not satisfy the ingestion contract")
	// ErrUnknownSite reports a page view for a site that does not exist, or was
	// deleted while the page view was queued.
	ErrUnknownSite = errors.New("page view references a site that does not exist")
	// ErrQueueFull reports a writer that already has as much work waiting as it
	// accepts. Retryable: the page view was refused rather than dropped, and
	// the caller may offer it again.
	ErrQueueFull = errors.New("write queue is full")
	// ErrUnavailable reports a write SQLite could not accept now — a writer
	// still busy after the configured timeout, or a full volume. Retryable, and
	// nothing partial was committed.
	ErrUnavailable = errors.New("database cannot accept writes right now")
	// ErrWrite reports a write that failed for a reason the caller cannot act
	// on. The transaction was rolled back, so no part of the event was kept.
	ErrWrite = errors.New("write failed")
)

// lifecycleClasses are the sentinels a sticky initialization failure can carry,
// most specific first.
var lifecycleClasses = []error{
	ErrClosed, ErrIntegrity, ErrConfiguration, ErrChecksum,
	ErrSchemaNewer, ErrMigration, ErrOrphaned, ErrUninitialized,
}

// ingestionClass reduces a lifecycle failure to its bare class.
//
// The remembered reason a database is not served is written for an operator: it
// names the stage that produced it and, for a driver failure, what SQLite said —
// "file is not a database (26)". Readiness and the startup log are where that
// belongs. A caller offering a page view learns only that the store is not
// usable and which kind of unusable it is, so the detail is dropped here rather
// than travelling out through the collection path.
//
// An unrecognised failure reduces to ErrUninitialized: whatever it was, the
// database never became usable.
func ingestionClass(err error) error {
	for _, class := range lifecycleClasses {
		if errors.Is(err, class) {
			return class
		}
	}
	return ErrUninitialized
}

// wrap builds a failure from its class, the lifecycle stage that produced it,
// and a rendering of the cause that carries no filesystem path.
func wrap(class error, stage string, cause error) error {
	return fmt.Errorf("%w: %s: %s", class, stage, sanitize(cause))
}

// describe builds a failure that has no underlying error, only a fact. The
// detail must never contain a configured value.
func describe(class error, stage, detail string) error {
	return fmt.Errorf("%w: %s: %s", class, stage, detail)
}

// sanitize renders err without the filesystem path it may carry.
//
// Storage errors reach two places that must not disclose where a deployment
// keeps its data: the operator log, which the service keeps free of configured
// values, and the readiness diagnostics derived from it. The operating system
// reports a failed file operation as its operation plus the path; keeping the
// operation and dropping the path preserves what makes the failure actionable
// — mkdir versus open, EACCES versus EROFS — without the value.
//
// SQLite's own messages are left intact: they name result codes, tables, and
// columns rather than paths, rows, or configuration.
func sanitize(err error) string {
	if err == nil {
		return "<nil>"
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Op + ": " + pathErr.Err.Error()
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Op + ": " + linkErr.Err.Error()
	}
	var syscallErr *os.SyscallError
	if errors.As(err, &syscallErr) {
		return syscallErr.Syscall + ": " + syscallErr.Err.Error()
	}
	return err.Error()
}
