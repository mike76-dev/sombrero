package stores

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
)

// initSQL creates the tables of an empty database.
//
//go:embed init.sql
var initSQL string

// schemaVersion is the version of the schema that init.sql creates. A change to
// the schema bumps it and has to upgrade the databases at the previous version.
const schemaVersion = 1

// prepareSchema brings the database to the schema this server works with,
// creating the tables in an empty one.
func prepareSchema(ctx context.Context, tx pgx.Tx) error {
	// Two servers starting on the same empty database must not both create it.
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

		if _, err := tx.Exec(ctx, "INSERT INTO schema_version (version) VALUES ($1)", schemaVersion); err != nil {
			return fmt.Errorf("failed to record the schema version: %w", err)
		}
	case err != nil:
		return fmt.Errorf("failed to read the schema version: %w", err)
	case version != schemaVersion:
		return fmt.Errorf("the database schema is at version %d, but this server works with version %d", version, schemaVersion)
	}

	return nil
}
