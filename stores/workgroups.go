package stores

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Workgroup represents a workgroup that can contain multiple accounts.
type Workgroup struct {
	ID   int       `json:"id"`
	UUID uuid.UUID `json:"uuid"`
}

// GetWorkgroupByID tries to retrieve the workgroup by its ID.
func (db *Database) GetWorkgroupByID(id int) (wg Workgroup, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT uuid
			FROM workgroups
			WHERE id = $1
		`
		var u uuid.UUID
		err = tx.QueryRow(ctx, query, id).Scan(&u)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve workgroup: %w", err)
		}
		wg = Workgroup{id, u}
		return nil
	})
	return
}

// FindWorkgroup tries to retrieve the workgroup by its UUID.
func (db *Database) FindWorkgroup(u uuid.UUID) (wg Workgroup, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT id
			FROM workgroups
			WHERE uuid = $1
		`
		var id int
		err = tx.QueryRow(ctx, query, u[:]).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve workgroup: %w", err)
		}
		wg = Workgroup{id, u}
		return nil
	})
	return
}

// AddWorkgroup adds a new workgroup to the database.
func (db *Database) AddWorkgroup(wg Workgroup) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			INSERT INTO workgroups (uuid)
			VALUES ($1)
		`
		_, err := tx.Exec(ctx, query, wg.UUID[:])
		if err != nil {
			return fmt.Errorf("failed to add workgroup: %w", err)
		}
		return nil
	})
}

// RemoveWorkgroup removes the specified workgroup and all associated accounts from the database.
func (db *Database) RemoveWorkgroup(wg Workgroup) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			DELETE FROM workgroups
			WHERE id = $1
		`
		_, err := tx.Exec(ctx, query, wg.ID)
		if err != nil {
			return fmt.Errorf("failed to remove workgroup: %w", err)
		}
		return nil
	})
}
