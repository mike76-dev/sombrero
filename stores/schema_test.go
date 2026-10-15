package stores

import (
	"context"
	_ "embed"
	"slices"
	"strings"
	"testing"
)

// schemaV1 is init.sql as it was at version 1, before there were any migrations. Never edited.
//
//go:embed testdata/schema_v1.sql
var schemaV1 string

// TestSchema verifies that the server sets the schema up itself, migrates an
// older one and refuses a database whose schema it doesn't know.
func TestSchema(t *testing.T) {
	ctx := context.Background()

	version := func(t *testing.T, db *Database) int {
		t.Helper()
		var v int
		if err := db.pool.QueryRow(ctx, "SELECT version FROM schema_version").Scan(&v); err != nil {
			t.Fatalf("reading the schema version: %v", err)
		}
		return v
	}

	exec := func(t *testing.T, db *Database, sql string) {
		t.Helper()
		if _, err := db.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	exists := func(t *testing.T, db *Database, table string) bool {
		t.Helper()
		var ok bool
		if err := db.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&ok); err != nil {
			t.Fatalf("looking for %s: %v", table, err)
		}
		return ok
	}

	// withMigrations appends to the real migrations for the length of the test.
	withMigrations := func(t *testing.T, extra ...migration) {
		t.Helper()
		old := migrations
		migrations = append(slices.Clone(old), extra...)
		t.Cleanup(func() { migrations = old })
	}

	t.Run("an empty database gets the tables and the version", func(t *testing.T) {
		db := NewTestStore(t, ctx)
		defer db.Close()

		if v := version(t, db); v != schemaVersion() {
			t.Fatalf("schema version: want %d, got %d", schemaVersion(), v)
		}
		addShare(t, db, "idx")
	})

	t.Run("reopening a database keeps what it holds", func(t *testing.T) {
		db := NewTestStore(t, ctx)
		defer db.Close()
		addShare(t, db, "idx")

		if err := db.txn(prepareSchema); err != nil {
			t.Fatalf("prepareSchema: %v", err)
		}
		if _, err := db.GetShare("idx"); err != nil {
			t.Fatalf("GetShare after reopening: %v", err)
		}
		if v := version(t, db); v != schemaVersion() {
			t.Fatalf("schema version: want %d, got %d", schemaVersion(), v)
		}
	})

	// A database set up by hand from init.sql has the tables but no version.
	t.Run("a database set up by hand is taken over", func(t *testing.T) {
		db := NewTestStore(t, ctx)
		defer db.Close()
		addShare(t, db, "idx")
		exec(t, db, "DROP TABLE schema_version")

		if err := db.txn(prepareSchema); err != nil {
			t.Fatalf("prepareSchema: %v", err)
		}
		if _, err := db.GetShare("idx"); err != nil {
			t.Fatalf("GetShare after taking over: %v", err)
		}
		if v := version(t, db); v != schemaVersion() {
			t.Fatalf("schema version: want %d, got %d", schemaVersion(), v)
		}
	})

	t.Run("a newer schema is refused", func(t *testing.T) {
		db := NewTestStore(t, ctx)
		defer db.Close()
		exec(t, db, "UPDATE schema_version SET version = version + 1")

		if err := db.txn(prepareSchema); err == nil {
			t.Fatal("prepareSchema accepted a schema newer than the server's")
		}
	})

	t.Run("an older schema is migrated step by step", func(t *testing.T) {
		db := NewTestStore(t, ctx)
		defer db.Close()
		addShare(t, db, "idx")
		withMigrations(t,
			migration{name: "add a", sql: "CREATE TABLE a (x INT PRIMARY KEY)"},
			migration{name: "add b", sql: "CREATE TABLE b (x INT REFERENCES a (x))"},
		)

		if err := db.txn(prepareSchema); err != nil {
			t.Fatalf("prepareSchema: %v", err)
		}
		if !exists(t, db, "a") || !exists(t, db, "b") {
			t.Fatal("the migrations were not applied")
		}
		if _, err := db.GetShare("idx"); err != nil {
			t.Fatalf("GetShare after migrating: %v", err)
		}
		if v := version(t, db); v != schemaVersion() {
			t.Fatalf("schema version: want %d, got %d", schemaVersion(), v)
		}
	})

	t.Run("a failed migration leaves the database as it was", func(t *testing.T) {
		db := NewTestStore(t, ctx)
		defer db.Close()
		before := version(t, db)
		withMigrations(t,
			migration{name: "add a", sql: "CREATE TABLE a (x INT)"},
			migration{name: "broken", sql: "ALTER TABLE no_such_table ADD COLUMN x INT"},
		)

		if err := db.txn(prepareSchema); err == nil {
			t.Fatal("prepareSchema accepted a failed migration")
		}
		if exists(t, db, "a") {
			t.Fatal("the migration before the failed one was kept")
		}
		if v := version(t, db); v != before {
			t.Fatalf("schema version: want %d, got %d", before, v)
		}
	})
}

// TestMigrations verifies that the version 1 schema, migrated, is the schema
// init.sql creates. It fails on a change made to one of them only.
func TestMigrations(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()
	want := schemaSnapshot(t, ctx, db)

	const setup = `
		DROP SCHEMA public CASCADE;
		CREATE SCHEMA public;
		CREATE TABLE schema_version (version INT NOT NULL);
		INSERT INTO schema_version (version) VALUES (1);
	`
	if _, err := db.pool.Exec(ctx, setup+schemaV1); err != nil {
		t.Fatalf("setting up version 1: %v", err)
	}
	if err := db.txn(prepareSchema); err != nil {
		t.Fatalf("prepareSchema: %v", err)
	}
	got := schemaSnapshot(t, ctx, db)

	for _, line := range want {
		if !slices.Contains(got, line) {
			t.Errorf("init.sql has, the migrations don't:\n\t%s", line)
		}
	}
	for _, line := range got {
		if !slices.Contains(want, line) {
			t.Errorf("the migrations have, init.sql doesn't:\n\t%s", line)
		}
	}
}

// schemaSnapshot lists the public schema, one sorted line per object. Column order is
// left out, since a migration can only append a column.
func schemaSnapshot(t *testing.T, ctx context.Context, db *Database) []string {
	t.Helper()

	const query = `
		SELECT 'column ' || c.relname || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
			|| CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END
			|| COALESCE(' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid), '')
			|| ' STORAGE ' || a.attstorage::text
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE c.relnamespace = 'public'::regnamespace
			AND c.relkind IN ('r', 'v', 'm')
			AND a.attnum > 0
			AND NOT a.attisdropped
		UNION ALL
		SELECT 'table ' || c.relname
			|| ' WITH (' || COALESCE(array_to_string(c.reloptions, ', '), '') || ')'
			|| ' TOAST (' || COALESCE(array_to_string(tc.reloptions, ', '), '') || ')'
		FROM pg_class c
		LEFT JOIN pg_class tc ON tc.oid = c.reltoastrelid
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r'
		UNION ALL
		SELECT 'constraint ' || conrelid::regclass::text || '.' || conname || ' ' || pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE connamespace = 'public'::regnamespace
		UNION ALL
		SELECT 'index ' || indexdef
		FROM pg_indexes
		WHERE schemaname = 'public'
		UNION ALL
		SELECT 'sequence ' || sequencename || ' ' || data_type::text
		FROM pg_sequences
		WHERE schemaname = 'public'
		UNION ALL
		SELECT 'function ' || pg_get_functiondef(oid)
		FROM pg_proc
		WHERE pronamespace = 'public'::regnamespace
		UNION ALL
		SELECT 'view ' || viewname || ' ' || definition
		FROM pg_views
		WHERE schemaname = 'public'
		UNION ALL
		SELECT 'trigger ' || pg_get_triggerdef(tg.oid)
		FROM pg_trigger tg
		JOIN pg_class c ON c.oid = tg.tgrelid
		WHERE c.relnamespace = 'public'::regnamespace AND NOT tg.tgisinternal
		ORDER BY 1
	`

	rows, err := db.pool.Query(ctx, query)
	if err != nil {
		t.Fatalf("taking a snapshot of the schema: %v", err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("reading the snapshot: %v", err)
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the snapshot: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("the snapshot is empty")
	}
	return lines
}
