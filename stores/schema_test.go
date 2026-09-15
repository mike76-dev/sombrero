package stores

import (
	"context"
	"testing"
)

// TestSchema verifies that the server sets the schema up itself and refuses a
// database whose schema it doesn't know.
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

	t.Run("an empty database gets the tables and the version", func(t *testing.T) {
		db := NewTestStore(t, ctx)
		defer db.Close()

		if v := version(t, db); v != schemaVersion {
			t.Fatalf("schema version: want %d, got %d", schemaVersion, v)
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
		if v := version(t, db); v != schemaVersion {
			t.Fatalf("schema version: want %d, got %d", schemaVersion, v)
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
		if v := version(t, db); v != schemaVersion {
			t.Fatalf("schema version: want %d, got %d", schemaVersion, v)
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
}
