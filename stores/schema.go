package stores

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
)

// initSQL creates the tables of an empty database at the current version.
//
//go:embed init.sql
var initSQL string

// migration upgrades the schema by one version.
type migration struct {
	name string
	sql  string
}

// migrations upgrade an existing database, the first one from version 1 to 2. A schema change
// goes into init.sql and is appended here; TestMigrations checks that the two agree.
var migrations = []migration{
	{name: "vacuum the buffers eagerly", sql: `
		ALTER TABLE buffers SET (
			toast.autovacuum_vacuum_scale_factor = 0,
			toast.autovacuum_vacuum_threshold = 10000,
			toast.autovacuum_vacuum_cost_limit = 2000
		)
	`},
	{name: "let a share opt out of backups", sql: `
		ALTER TABLE shares ADD COLUMN skip_backup BOOLEAN NOT NULL DEFAULT FALSE
	`},
	{name: "remember how far a slab was filled", sql: `
		CREATE TABLE slabs (
			share_name TEXT NOT NULL,
			workgroup INT NOT NULL REFERENCES workgroups(id) ON DELETE CASCADE,
			slab_key BYTEA NOT NULL,
			filled BIGINT NOT NULL,
			CONSTRAINT slabs_share_fk FOREIGN KEY (share_name) REFERENCES shares(share_name) ON DELETE CASCADE,
			CONSTRAINT slabs_unique UNIQUE (share_name, workgroup, slab_key),
			CONSTRAINT slabs_key_length CHECK (octet_length(slab_key) = 32)
		);
		CREATE INDEX idx_slabs_workgroup ON slabs (workgroup);
		INSERT INTO slabs (share_name, workgroup, slab_key, filled)
		SELECT o.share_name, o.workgroup, m.slab_key, MAX(m.data_offset + m.data_length)
		FROM metadata m
		JOIN objects o ON o.id = m.object_id
		WHERE m.slab_key IS NOT NULL
		GROUP BY o.share_name, o.workgroup, m.slab_key
	`},
}

// schemaVersion is the version of the schema that init.sql creates.
func schemaVersion() int {
	return 1 + len(migrations)
}

// prepareSchema brings the database to the schema this server works with,
// creating the tables in an empty one and migrating an older one.
func prepareSchema(ctx context.Context, tx pgx.Tx) error {
	// Two servers starting on the same database must not both create or migrate it.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('sombrero_schema'))"); err != nil {
		return fmt.Errorf("failed to lock the schema: %w", err)
	}

	if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_version (version INT NOT NULL)"); err != nil {
		return fmt.Errorf("failed to create the schema version table: %w", err)
	}

	var version int
	err := tx.QueryRow(ctx, "SELECT version FROM schema_version").Scan(&version)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// A database set up by hand from init.sql has the tables, only no version.
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT to_regclass('shares') IS NOT NULL").Scan(&exists); err != nil {
			return fmt.Errorf("failed to look for the tables: %w", err)
		}
		if !exists {
			if _, err := tx.Exec(ctx, initSQL); err != nil {
				return fmt.Errorf("failed to create the tables: %w", err)
			}
			log.Println("Created the database tables")
		}

		if _, err := tx.Exec(ctx, "INSERT INTO schema_version (version) VALUES ($1)", schemaVersion()); err != nil {
			return fmt.Errorf("failed to record the schema version: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("failed to read the schema version: %w", err)
	case version < 1 || version > schemaVersion():
		return fmt.Errorf("the database schema is at version %d, but this server works with version %d", version, schemaVersion())
	}

	// All in the one transaction: a failed migration leaves the database as it was.
	for ; version < schemaVersion(); version++ {
		m := migrations[version-1]
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return fmt.Errorf("failed to migrate the schema to version %d (%s): %w", version+1, m.name, err)
		}
		if _, err := tx.Exec(ctx, "UPDATE schema_version SET version = $1", version+1); err != nil {
			return fmt.Errorf("failed to record the schema version: %w", err)
		}
		log.Printf("Migrated the database schema to version %d: %s", version+1, m.name)
	}

	return nil
}
