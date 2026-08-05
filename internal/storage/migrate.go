package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
)

// migrationDir holds the ordered SQL migrations, embedded so that a binary
// always carries the schema it expects and can never be pointed at a different
// set of files at runtime.
//
//go:embed migrations/*.sql
var migrationDir embed.FS

// bootstrapSQL creates the migration ledger.
//
// It is not itself a migration: the runner has to read the ledger before it can
// decide what is pending. The statement is idempotent, and its shape matches
// the reference schema, so bootstrapping plus migration 1 produces the accepted
// data model.
const bootstrapSQL = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version       INTEGER PRIMARY KEY,
    checksum      TEXT NOT NULL CHECK (length(checksum) = 64),
    applied_at_ms INTEGER NOT NULL CHECK (applied_at_ms >= 0)
) STRICT`

// migrationName is the required file name shape: a zero-padded version, a
// snake_case description, and the .sql extension.
var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// migration is one embedded SQL file.
type migration struct {
	version int
	name    string
	sql     string
	// checksum is the SHA-256 of the file's bytes, hex encoded. It is recorded
	// when the migration is applied and compared on every later startup, so
	// that editing an applied migration is caught instead of silently
	// producing two different schemas that claim the same version.
	checksum string
}

// appliedMigration is one row of the ledger.
type appliedMigration struct {
	version   int
	checksum  string
	appliedAt int64
}

// embeddedMigrations is the migration set this binary understands.
var embeddedMigrations = mustLoadMigrations()

func mustLoadMigrations() []migration {
	loaded, err := loadMigrations(migrationDir, "migrations")
	if err != nil {
		// The set is compiled in: a malformed one is a build defect that no
		// deployment could recover from, and failing here fails every test.
		panic("storage: " + err.Error())
	}
	return loaded
}

// loadMigrations reads and orders a migration set, rejecting anything that
// would make "version N" ambiguous.
func loadMigrations(files fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	loaded := make([]migration, 0, len(entries))
	for _, entry := range entries {
		match := migrationName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("migration %q must be named NNNN_description.sql", entry.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("migration %q has an unreadable version: %w", entry.Name(), err)
		}
		body, err := fs.ReadFile(files, path.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(body)
		loaded = append(loaded, migration{
			version:  version,
			name:     entry.Name(),
			sql:      string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(loaded, func(i, j int) bool { return loaded[i].version < loaded[j].version })
	for i, m := range loaded {
		// Versions are consecutive from 1 so that "newer than this binary"
		// is decidable from the highest version alone.
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %q should be version %d", m.name, i+1)
		}
	}
	if len(loaded) == 0 {
		return nil, fmt.Errorf("no migrations are embedded")
	}
	return loaded, nil
}

// migrate brings the database up to the latest embedded version and returns the
// version it is now at.
func (d *Database) migrate(ctx context.Context, db *sql.DB, set []migration) (int, error) {
	if _, err := db.ExecContext(ctx, bootstrapSQL); err != nil {
		return 0, wrap(classify(err), "bootstrap migration ledger", err)
	}

	applied, err := readAppliedMigrations(ctx, db)
	if err != nil {
		return 0, err
	}

	latest := set[len(set)-1].version
	known := make(map[int]migration, len(set))
	for _, m := range set {
		known[m.version] = m
	}

	// A database written by a newer binary is not migrated, and not served:
	// this binary cannot know what the newer schema means, and writing to it
	// with the older understanding is how silent corruption starts.
	for _, record := range applied {
		if record.version > latest {
			return 0, describe(ErrSchemaNewer, "compare schema version",
				fmt.Sprintf("database is at version %d, this binary understands %d", record.version, latest))
		}
		expected, ok := known[record.version]
		switch {
		case !ok:
			return 0, describe(ErrChecksum, "verify applied migrations",
				fmt.Sprintf("version %d is recorded but not embedded", record.version))
		case expected.checksum != record.checksum:
			return 0, describe(ErrChecksum, "verify applied migrations",
				fmt.Sprintf("version %d was applied from different SQL", record.version))
		}
	}

	for _, m := range set {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := d.apply(ctx, db, m); err != nil {
			return 0, err
		}
	}
	return latest, nil
}

// apply runs one migration and records it in the same transaction. SQLite makes
// schema changes transactional, so a failure anywhere in the file leaves the
// previous committed schema and the previous ledger untouched.
func (d *Database) apply(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return wrap(classify(err), "begin migration "+m.name, err)
	}
	// Rolling back a committed transaction is a no-op; this is the path that
	// undoes a partially applied file.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return wrap(ErrMigration, "apply migration "+m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, checksum, applied_at_ms) VALUES (?, ?, ?)",
		m.version, m.checksum, d.now().UTC().UnixMilli(),
	); err != nil {
		return wrap(ErrMigration, "record migration "+m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return wrap(ErrMigration, "commit migration "+m.name, err)
	}
	return nil
}

// readAppliedMigrations returns the ledger keyed by version.
func readAppliedMigrations(ctx context.Context, db *sql.DB) (map[int]appliedMigration, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT version, checksum, applied_at_ms FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, wrap(classify(err), "read migration ledger", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[int]appliedMigration)
	for rows.Next() {
		var record appliedMigration
		if err := rows.Scan(&record.version, &record.checksum, &record.appliedAt); err != nil {
			return nil, wrap(classify(err), "read migration ledger", err)
		}
		applied[record.version] = record
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(classify(err), "read migration ledger", err)
	}
	return applied, nil
}

// recordedVersion returns the highest version in the ledger, or zero when the
// ledger is empty.
func recordedVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&version)
	if err != nil {
		return 0, wrap(classify(err), "read schema version", err)
	}
	return version, nil
}
