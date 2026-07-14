package stores

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Workgroup represents a workgroup that can contain multiple accounts.
type Workgroup struct {
	ID            int       `json:"id"`
	UUID          uuid.UUID `json:"uuid"`
	Name          string    `json:"name,omitempty"`
	PublicDirs    []string  `json:"publicDirs,omitempty"`
	CaseSensitive bool      `json:"caseSensitive,omitempty"`
}

// publicDirsFromDB converts the semicolon-separated DB value to a string slice.
func publicDirsFromDB(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ";")
}

// publicDirsToDB converts a string slice to the semicolon-separated DB value.
func publicDirsToDB(dirs []string) string {
	return strings.Join(dirs, ";")
}

// GetWorkgroupByID tries to retrieve the workgroup by its ID.
func (db *Database) GetWorkgroupByID(id int) (wg Workgroup, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT uuid, name, public_dirs, case_sensitive
			FROM workgroups
			WHERE id = $1
		`
		var u uuid.UUID
		var name *string
		var publicDirs string
		var caseSensitive bool
		err = tx.QueryRow(ctx, query, id).Scan(&u, &name, &publicDirs, &caseSensitive)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve workgroup: %w", err)
		}
		wg = Workgroup{ID: id, UUID: u, PublicDirs: publicDirsFromDB(publicDirs), CaseSensitive: caseSensitive}
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
			SELECT id, name, public_dirs, case_sensitive
			FROM workgroups
			WHERE uuid = $1
		`
		var id int
		var name *string
		var publicDirs string
		var caseSensitive bool
		err = tx.QueryRow(ctx, query, u[:]).Scan(&id, &name, &publicDirs, &caseSensitive)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve workgroup: %w", err)
		}
		wg = Workgroup{ID: id, UUID: u, PublicDirs: publicDirsFromDB(publicDirs), CaseSensitive: caseSensitive}
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
			SELECT id, uuid, public_dirs, case_sensitive
			FROM workgroups
			WHERE name = $1
		`
		var id int
		var u uuid.UUID
		var publicDirs string
		var caseSensitive bool
		err = tx.QueryRow(ctx, query, name).Scan(&id, &u, &publicDirs, &caseSensitive)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to retrieve workgroup: %w", err)
		}
		wg = Workgroup{ID: id, UUID: u, Name: name, PublicDirs: publicDirsFromDB(publicDirs), CaseSensitive: caseSensitive}
		return nil
	})
	return
}

// GetWorkgroups lists all workgroups.
func (db *Database) GetWorkgroups() (wgs []Workgroup, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT id, uuid, name, public_dirs, case_sensitive
			FROM workgroups
			ORDER BY id
		`
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return fmt.Errorf("failed to retrieve workgroups: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int
			var u uuid.UUID
			var name *string
			var publicDirs string
			var caseSensitive bool
			if err := rows.Scan(&id, &u, &name, &publicDirs, &caseSensitive); err != nil {
				return fmt.Errorf("failed to retrieve workgroups: %w", err)
			}
			wg := Workgroup{ID: id, UUID: u, PublicDirs: publicDirsFromDB(publicDirs), CaseSensitive: caseSensitive}
			if name != nil {
				wg.Name = *name
			}
			wgs = append(wgs, wg)
		}
		return nil
	})
	return
}

// AddWorkgroup adds a new workgroup to the database.
func (db *Database) AddWorkgroup(wg Workgroup) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			INSERT INTO workgroups (uuid, name, public_dirs, case_sensitive)
			VALUES ($1, $2, $3, $4)
		`
		var name any
		if wg.Name != "" {
			name = wg.Name
		}
		_, err := tx.Exec(ctx, query, wg.UUID[:], name, publicDirsToDB(wg.PublicDirs), wg.CaseSensitive)
		if err != nil {
			return fmt.Errorf("failed to add workgroup: %w", err)
		}
		return nil
	})
}

// UpdateWorkgroup updates the public_dirs and case_sensitive settings of a workgroup.
func (db *Database) UpdateWorkgroup(wg Workgroup) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			UPDATE workgroups
			SET public_dirs = $2, case_sensitive = $3
			WHERE id = $1
		`
		tag, err := tx.Exec(ctx, query, wg.ID, publicDirsToDB(wg.PublicDirs), wg.CaseSensitive)
		if err != nil {
			return fmt.Errorf("failed to update workgroup: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("workgroup not found")
		}
		return nil
	})
}

// RemoveWorkgroup removes the specified workgroup and all associated accounts from the database.
func (db *Database) RemoveWorkgroup(wg Workgroup) error {
	accs, err := db.FindAccounts(wg.UUID.String())
	if err != nil {
		return err
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const connQuery = `
			SELECT share_name
			FROM connections
			WHERE workgroup = $1
		`
		rows, err := tx.Query(ctx, connQuery, wg.ID)
		if err != nil {
			return fmt.Errorf("failed to retrieve connections: %w", err)
		}
		var shareNames []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return fmt.Errorf("failed to retrieve connections: %w", err)
			}
			shareNames = append(shareNames, name)
		}
		rows.Close()

		const query = `
			DELETE FROM workgroups
			WHERE id = $1
		`
		if _, err := tx.Exec(ctx, query, wg.ID); err != nil {
			return fmt.Errorf("failed to remove workgroup: %w", err)
		}

		for _, name := range shareNames {
			if err := db.shares.RemoveConnection(wg, Share{Name: name}); err != nil {
				return fmt.Errorf("failed to disconnect share: %w", err)
			}
		}
		for _, acc := range accs {
			db.shares.RemoveAccess(acc)
		}
		return nil
	})
}
