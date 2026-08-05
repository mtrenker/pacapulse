package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// referenceSchemaPath is the accepted architecture artifact the runtime schema
// has to agree with. It is never executed by the service.
const referenceSchemaPath = "../../docs/architecture/data-model.sql"

// TestMigratedSchemaMatchesTheReference is the check that keeps the accepted
// data model and the shipped migrations from drifting apart. It compares the
// objects SQLite ends up with, not the text of the files, so formatting is free
// to differ while constraints, indexes, and column definitions are not.
func TestMigratedSchemaMatchesTheReference(t *testing.T) {
	t.Parallel()

	reference, err := os.ReadFile(referenceSchemaPath)
	if err != nil {
		t.Fatalf("read reference schema: %v", err)
	}

	referencePath := filepath.Join(t.TempDir(), "reference.db")
	referenceDB, err := sql.Open(driverName, "file:"+referencePath)
	if err != nil {
		t.Fatalf("open reference database: %v", err)
	}
	defer func() { _ = referenceDB.Close() }()
	if _, err := referenceDB.Exec(string(reference)); err != nil {
		t.Fatalf("apply reference schema: %v", err)
	}

	migrated := open(t, filepath.Join(t.TempDir(), "data"))
	handle, err := migrated.handle()
	if err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	want := schemaObjects(t, referenceDB)
	got := schemaObjects(t, handle)

	// The reference defines fourteen tables and seven indexes; anything much
	// smaller means the comparison read an empty database and proved nothing.
	if len(want) < 21 {
		t.Fatalf("the reference schema produced %d objects, want at least 21", len(want))
	}
	if len(got) != len(want) {
		t.Fatalf("migrated schema has %d objects, the reference has %d:\n%s\n%s",
			len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("schema object %d differs:\n migrated: %s\nreference: %s", i, got[i], want[i])
		}
	}
}

// schemaObjects returns every named object SQLite stores DDL for, normalised so
// that only its meaning is compared.
func schemaObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.Query("SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL")
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var objects []string
	for rows.Next() {
		var kind, name, statement string
		if err := rows.Scan(&kind, &name, &statement); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		objects = append(objects, kind+" "+name+": "+normaliseDDL(statement))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	sort.Strings(objects)
	return objects
}

// normaliseDDL collapses whitespace and drops the idempotence clause the
// migration ledger is bootstrapped with, which is the one intended difference
// between the two schemas.
func normaliseDDL(statement string) string {
	statement = strings.Join(strings.Fields(statement), " ")
	return strings.ReplaceAll(statement, "IF NOT EXISTS ", "")
}

// TestFailedMigrationPreservesTheCommittedSchema covers a migration that is
// wrong rather than merely new: nothing it contains may survive, and the
// database must stay at the version it already had.
func TestFailedMigrationPreservesTheCommittedSchema(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	db := open(t, dir)
	handle, err := db.handle()
	if err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	broken := append(append([]migration{}, embeddedMigrations...), migration{
		version: len(embeddedMigrations) + 1,
		name:    fmt.Sprintf("%04d_broken.sql", len(embeddedMigrations)+1),
		sql: "CREATE TABLE experiment (id INTEGER PRIMARY KEY) STRICT;\n" +
			"THIS IS NOT SQL;\n",
		checksum: strings.Repeat("a", 64),
	})

	_, err = db.migrate(context.Background(), handle, broken)
	if !errors.Is(err, ErrMigration) {
		t.Fatalf("migrate() error = %v, want %v", err, ErrMigration)
	}

	if got := query[int](t, db, "SELECT coalesce(max(version), 0) FROM schema_migrations"); got != len(embeddedMigrations) {
		t.Errorf("schema version = %d after a failed migration, want %d", got, len(embeddedMigrations))
	}
	if got := query[int](t, db, "SELECT count(*) FROM sqlite_master WHERE name = 'experiment'"); got != 0 {
		t.Error("the failed migration left a table behind")
	}
	// The schema it did not damage is still the one the service can serve.
	if err := db.Check(context.Background()); err != nil {
		t.Errorf("Check() error = %v, want the previous schema to remain usable", err)
	}
}

// TestSucceedingMigrationIsRecorded proves the runner applies and records a
// later version rather than only ever bootstrapping version 1.
func TestSucceedingMigrationIsRecorded(t *testing.T) {
	t.Parallel()

	db := open(t, filepath.Join(t.TempDir(), "data"))
	handle, err := db.handle()
	if err != nil {
		t.Fatalf("handle() error = %v", err)
	}

	next := len(embeddedMigrations) + 1
	extended := append(append([]migration{}, embeddedMigrations...), migration{
		version:  next,
		name:     fmt.Sprintf("%04d_extra.sql", next),
		sql:      "CREATE TABLE extra (id INTEGER PRIMARY KEY) STRICT;",
		checksum: strings.Repeat("b", 64),
	})

	version, err := db.migrate(context.Background(), handle, extended)
	if err != nil {
		t.Fatalf("migrate() error = %v", err)
	}
	if version != next {
		t.Errorf("migrate() = %d, want %d", version, next)
	}
	got := query[string](t, db, fmt.Sprintf("SELECT checksum FROM schema_migrations WHERE version = %d", next))
	if want := strings.Repeat("b", 64); got != want {
		t.Errorf("recorded checksum = %q, want %q", got, want)
	}
}

// TestEmbeddedMigrationsAreOrdered checks the set the binary ships with.
func TestEmbeddedMigrationsAreOrdered(t *testing.T) {
	t.Parallel()

	if len(embeddedMigrations) == 0 {
		t.Fatal("no migrations are embedded")
	}
	for i, m := range embeddedMigrations {
		if m.version != i+1 {
			t.Errorf("migration %q has version %d, want %d", m.name, m.version, i+1)
		}
		if len(m.checksum) != 64 {
			t.Errorf("migration %q checksum = %q, want 64 hex characters", m.name, m.checksum)
		}
		if strings.TrimSpace(m.sql) == "" {
			t.Errorf("migration %q is empty", m.name)
		}
	}
}

// TestLoadMigrationsRejectsAnAmbiguousSet covers the sets that would make
// "version N" mean more than one thing.
func TestLoadMigrationsRejectsAnAmbiguousSet(t *testing.T) {
	t.Parallel()

	tests := map[string]fstest.MapFS{
		"unnamed file": {
			"m/initial.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		},
		"gap in the sequence": {
			"m/0001_initial.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			"m/0003_later.sql":   &fstest.MapFile{Data: []byte("SELECT 1;")},
		},
		"does not start at one": {
			"m/0002_initial.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		},
	}

	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := loadMigrations(files, "m"); err == nil {
				t.Fatal("loadMigrations() error = nil, want a rejection")
			}
		})
	}
}

// TestSchemaConstraintsRejectInvalidRows executes the accepted constraints
// against representative rows, including the byte limits and the overflow
// contract the aggregate tables encode.
func TestSchemaConstraintsRejectInvalidRows(t *testing.T) {
	t.Parallel()

	db := open(t, filepath.Join(t.TempDir(), "data"))
	handle, err := db.handle()
	if err != nil {
		t.Fatalf("handle() error = %v", err)
	}
	ctx := context.Background()

	const site = "11111111-1111-4111-8111-111111111111"
	const org = "22222222-2222-4222-8222-222222222222"
	for _, statement := range []string{
		fmt.Sprintf("INSERT INTO organizations (id, name, created_at_ms) VALUES ('%s', 'Example', 0)", org),
		fmt.Sprintf("INSERT INTO sites (id, organization_id, name, collection_key, canonical_host, created_at_ms) "+
			"VALUES ('%s', '%s', 'Example', 'collection-key-2222222', 'example.com', 0)", site, org),
	} {
		if _, err := handle.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// A UTF-8 path at its byte limit is accepted; one byte more is not. The
	// limits are byte limits, so a multi-byte character has to count for more
	// than one.
	longestPath := "/" + strings.Repeat("a", 511)
	accepted := []string{
		fmt.Sprintf("INSERT INTO page_view_events (site_id, event_id, received_at_ms, day, path) VALUES "+
			"('%s', '33333333-3333-4333-8333-333333333333', 1, '2026-03-14', '%s')", site, longestPath),
		fmt.Sprintf("INSERT INTO daily_page_stats (site_id, day, path) VALUES ('%s', '2026-03-14', '/pricing')", site),
		fmt.Sprintf("INSERT INTO daily_page_stats (site_id, day, path, overflow) VALUES ('%s', '2026-03-14', '', 1)", site),
		fmt.Sprintf("INSERT INTO daily_campaign_stats (site_id, day, campaign_source, campaign_medium, campaign_name) "+
			"VALUES ('%s', '2026-03-14', 'newsletter', '', '')", site),
	}
	for _, statement := range accepted {
		if _, err := handle.ExecContext(ctx, statement); err != nil {
			t.Errorf("a valid row was rejected: %v", err)
		}
	}

	rejected := map[string]string{
		"path over the byte limit": fmt.Sprintf(
			"INSERT INTO page_view_events (site_id, event_id, received_at_ms, day, path) VALUES "+
				"('%s', '44444444-4444-4444-8444-444444444444', 1, '2026-03-14', '/%s')", site, strings.Repeat("a", 512)),
		"multi-byte path over the byte limit": fmt.Sprintf(
			"INSERT INTO page_view_events (site_id, event_id, received_at_ms, day, path) VALUES "+
				"('%s', '55555555-5555-4555-8555-555555555555', 1, '2026-03-14', '/%s')", site, strings.Repeat("ü", 256)),
		"path carrying a query": fmt.Sprintf(
			"INSERT INTO page_view_events (site_id, event_id, received_at_ms, day, path) VALUES "+
				"('%s', '66666666-6666-4666-8666-666666666666', 1, '2026-03-14', '/search?q=secret')", site),
		"day that is not a calendar shape": fmt.Sprintf(
			"INSERT INTO daily_site_stats (site_id, day) VALUES ('%s', '14-03-2026')", site),
		"unknown site": "INSERT INTO daily_site_stats (site_id, day) VALUES " +
			"('99999999-9999-4999-8999-999999999999', '2026-03-14')",
		"negative counter": fmt.Sprintf(
			"INSERT INTO daily_site_stats (site_id, day, page_views) VALUES ('%s', '2026-03-15', -1)", site),
		"overflow row carrying a dimension": fmt.Sprintf(
			"INSERT INTO daily_page_stats (site_id, day, path, overflow) VALUES ('%s', '2026-03-15', '/pricing', 1)", site),
		"non-overflow row without a dimension": fmt.Sprintf(
			"INSERT INTO daily_campaign_stats (site_id, day, campaign_source, campaign_medium, campaign_name) "+
				"VALUES ('%s', '2026-03-15', '', '', '')", site),
		"campaign value over the byte limit": fmt.Sprintf(
			"INSERT INTO daily_campaign_stats (site_id, day, campaign_source, campaign_medium, campaign_name) "+
				"VALUES ('%s', '2026-03-15', '%s', '', '')", site, strings.Repeat("a", 101)),
		"visitor key of the wrong length": "INSERT INTO daily_site_visitors (stat_id, visitor_key) VALUES " +
			"(1, randomblob(16))",
		"visitor key without an aggregate row": "INSERT INTO daily_site_visitors (stat_id, visitor_key) VALUES " +
			"(987654, randomblob(32))",
	}
	for name, statement := range rejected {
		if _, err := handle.ExecContext(ctx, statement); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// Nothing the constraints rejected may leave the database inconsistent.
	if err := foreignKeyCheck(ctx, handle); err != nil {
		t.Errorf("foreignKeyCheck() error = %v", err)
	}
	if err := orphanCheck(ctx, handle); err != nil {
		t.Errorf("orphanCheck() error = %v", err)
	}
}
