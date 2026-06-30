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
	Name string    `json:"name,omitempty"`
}

// GetWorkgroupByID tries to retrieve the workgroup by its ID.
func (db *Database) GetWorkgroupByID(id int) (wg Workgroup, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT uuid, name
			FROM workgroups
			WHERE id = $1
		`
		var u uuid.UUID
		var name *string
		err = tx.QueryRow(ctx, query, id).Scan(&u, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve workgroup: %w", err)
		}
		wg = Workgroup{ID: id, UUID: u}
		if name != nil {
			wg.Name = *name
		}
		return nil
	})
	return
}

// FindWorkgroup tries to retrieve the workgroup by its UUID.
func (db *Database) FindWorkgroup(u uuid.UUID) (wg Workgroup, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT id, name
			FROM workgroups
			WHERE uuid = $1
		`
		var id int
		var name *string
		err = tx.QueryRow(ctx, query, u[:]).Scan(&id, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve workgroup: %w", err)
		}
		wg = Workgroup{ID: id, UUID: u}
		if name != nil {
			wg.Name = *name
		}
		return nil
	})
	return
}

// FindWorkgroupByName tries to retrieve the workgroup by its name.
func (db *Database) FindWorkgroupByName(name string) (wg Workgroup, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT id, uuid
			FROM workgroups
			WHERE name = $1
		`
		var id int
		var u uuid.UUID
		err = tx.QueryRow(ctx, query, name).Scan(&id, &u)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve workgroup: %w", err)
		}
		wg = Workgroup{ID: id, UUID: u, Name: name}
		return nil
	})
	return
}

// AddWorkgroup adds a new workgroup to the database.
func (db *Database) AddWorkgroup(wg Workgroup) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			INSERT INTO workgroups (uuid, name)
			VALUES ($1, $2)
		`
		var name any
		if wg.Name != "" {
			name = wg.Name
		}
		_, err := tx.Exec(ctx, query, wg.UUID[:], name)
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
