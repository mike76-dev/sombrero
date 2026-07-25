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

// PublicDir is a folder whose contents are visible to every member of the
// workgroup it belongs to. ReadOnly decides what the other members may do with
// the files placed into that folder: if it is set, only the account that owns a
// file may overwrite, rename over, or delete it, otherwise any member of the
// workgroup may. CaseSensitive decides whether Path has to match the folder
// name exactly.
type PublicDir struct {
	Path          string `json:"path"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
	CaseSensitive bool   `json:"caseSensitive,omitempty"`
}

// Matches reports whether the folder name refers to this public folder.
func (pd PublicDir) Matches(name string) bool {
	if pd.CaseSensitive {
		return pd.Path == name
	}
	return strings.EqualFold(pd.Path, name)
}

// Workgroup represents a workgroup that can contain multiple accounts.
type Workgroup struct {
	ID         int         `json:"id"`
	UUID       uuid.UUID   `json:"uuid"`
	Name       string      `json:"name,omitempty"`
	PublicDirs []PublicDir `json:"publicDirs,omitempty"`
}

// FindPublicDir returns the public folder of the workgroup matching the
// provided folder name, if there is one.
func (wg Workgroup) FindPublicDir(name string) (PublicDir, bool) {
	for _, dir := range wg.PublicDirs {
		if dir.Matches(name) {
			return dir, true
		}
	}
	return PublicDir{}, false
}

// loadPublicDirs retrieves the public folders of the specified workgroup.
func loadPublicDirs(ctx context.Context, tx pgx.Tx, id int) (dirs []PublicDir, err error) {
	const query = `
		SELECT path, read_only, case_sensitive
		FROM public_dirs
		WHERE workgroup = $1
		ORDER BY id
	`
	rows, err := tx.Query(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve public folders: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var dir PublicDir
		if err := rows.Scan(&dir.Path, &dir.ReadOnly, &dir.CaseSensitive); err != nil {
			return nil, fmt.Errorf("failed to retrieve public folders: %w", err)
		}
		dirs = append(dirs, dir)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("failed to retrieve public folders: %w", rows.Err())
	}

	return dirs, nil
}

// loadAllPublicDirs retrieves the public folders of every workgroup, keyed by
// the workgroup ID.
func loadAllPublicDirs(ctx context.Context, tx pgx.Tx) (map[int][]PublicDir, error) {
	const query = `
		SELECT workgroup, path, read_only, case_sensitive
		FROM public_dirs
		ORDER BY workgroup, id
	`
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve public folders: %w", err)
	}
	defer rows.Close()

	dirs := make(map[int][]PublicDir)
	for rows.Next() {
		var id int
		var dir PublicDir
		if err := rows.Scan(&id, &dir.Path, &dir.ReadOnly, &dir.CaseSensitive); err != nil {
			return nil, fmt.Errorf("failed to retrieve public folders: %w", err)
		}
		dirs[id] = append(dirs[id], dir)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("failed to retrieve public folders: %w", rows.Err())
	}

	return dirs, nil
}

// normalizePublicDirs drops the entries with an empty path and keeps only the
// first entry for each path, matching the first-match-wins rule of
// FindPublicDir and restampDirectories.
func normalizePublicDirs(dirs []PublicDir) []PublicDir {
	var out []PublicDir
	seen := make(map[string]bool)
	for _, dir := range dirs {
		if dir.Path == "" || seen[dir.Path] {
			continue
		}
		seen[dir.Path] = true
		out = append(out, dir)
	}
	return out
}

// savePublicDirs replaces the public folders of the specified workgroup.
func savePublicDirs(ctx context.Context, tx pgx.Tx, id int, dirs []PublicDir) error {
	const clearQuery = `
		DELETE FROM public_dirs
		WHERE workgroup = $1
	`
	if _, err := tx.Exec(ctx, clearQuery, id); err != nil {
		return fmt.Errorf("failed to clear public folders: %w", err)
	}

	const insertQuery = `
		INSERT INTO public_dirs (workgroup, path, read_only, case_sensitive)
		VALUES ($1, $2, $3, $4)
	`
	for _, dir := range normalizePublicDirs(dirs) {
		if _, err := tx.Exec(ctx, insertQuery, id, dir.Path, dir.ReadOnly, dir.CaseSensitive); err != nil {
			return fmt.Errorf("failed to add public folder: %w", err)
		}
	}

	return nil
}

// restampDirectories re-applies the public folders of a workgroup to the
// directories that already exist in it, so that changing the list also affects
// the folders created before the change. A directory whose name matches an
// entry of the list becomes visible to the whole workgroup and inherits the
// read-only flag of that entry; a directory that no longer matches any entry
// becomes private again. As in FindPublicDir, the first matching entry wins.
func restampDirectories(ctx context.Context, tx pgx.Tx, id int, dirs []PublicDir) error {
	dirs = normalizePublicDirs(dirs)
	paths := make([]string, 0, len(dirs))
	readOnly := make([]bool, 0, len(dirs))
	caseSensitive := make([]bool, 0, len(dirs))
	for _, dir := range dirs {
		paths = append(paths, dir.Path)
		readOnly = append(readOnly, dir.ReadOnly)
		caseSensitive = append(caseSensitive, dir.CaseSensitive)
	}

	// The name of a directory is its last path component, which is what
	// MakeDirectory matches against the public folders.
	const publicQuery = `
		UPDATE directories d
		SET private = FALSE, read_only = m.read_only
		FROM (
			SELECT DISTINCT ON (d.id) d.id, pd.read_only
			FROM directories d
			JOIN unnest($2::text[], $3::boolean[], $4::boolean[])
				WITH ORDINALITY AS pd(path, read_only, case_sensitive, ord)
				ON CASE
					WHEN pd.case_sensitive THEN d.name = pd.path
					ELSE lower(d.name) = lower(pd.path)
				END
			WHERE d.workgroup = $1
			ORDER BY d.id, pd.ord
		) m
		WHERE d.id = m.id
			AND (d.private OR d.read_only <> m.read_only)
	`
	if _, err := tx.Exec(ctx, publicQuery, id, paths, readOnly, caseSensitive); err != nil {
		return fmt.Errorf("failed to update public directories: %w", err)
	}

	const privateQuery = `
		UPDATE directories d
		SET private = TRUE, read_only = FALSE
		WHERE d.workgroup = $1
			AND (NOT d.private OR d.read_only)
			AND NOT EXISTS (
				SELECT 1
				FROM unnest($2::text[], $3::boolean[]) AS pd(path, case_sensitive)
				WHERE CASE
					WHEN pd.case_sensitive THEN d.name = pd.path
					ELSE lower(d.name) = lower(pd.path)
				END
			)
	`
	if _, err := tx.Exec(ctx, privateQuery, id, paths, caseSensitive); err != nil {
		return fmt.Errorf("failed to update private directories: %w", err)
	}

	return nil
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
		dirs, err := loadPublicDirs(ctx, tx, id)
		if err != nil {
			return err
		}
		wg = Workgroup{ID: id, UUID: u, PublicDirs: dirs}
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
		dirs, err := loadPublicDirs(ctx, tx, id)
		if err != nil {
			return err
		}
		wg = Workgroup{ID: id, UUID: u, PublicDirs: dirs}
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
		dirs, err := loadPublicDirs(ctx, tx, id)
		if err != nil {
			return err
		}
		wg = Workgroup{ID: id, UUID: u, Name: name, PublicDirs: dirs}
		return nil
	})
	return
}

// GetWorkgroups lists all workgroups.
func (db *Database) GetWorkgroups() (wgs []Workgroup, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT id, uuid, name
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
			if err := rows.Scan(&id, &u, &name); err != nil {
				return fmt.Errorf("failed to retrieve workgroups: %w", err)
			}
			wg := Workgroup{ID: id, UUID: u}
			if name != nil {
				wg.Name = *name
			}
			wgs = append(wgs, wg)
		}
		if rows.Err() != nil {
			return fmt.Errorf("failed to retrieve workgroups: %w", rows.Err())
		}
		rows.Close()

		dirs, err := loadAllPublicDirs(ctx, tx)
		if err != nil {
			return err
		}
		for i := range wgs {
			wgs[i].PublicDirs = dirs[wgs[i].ID]
		}
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
			RETURNING id
		`
		var name any
		if wg.Name != "" {
			name = wg.Name
		}
		var id int
		if err := tx.QueryRow(ctx, query, wg.UUID[:], name).Scan(&id); err != nil {
			return fmt.Errorf("failed to add workgroup: %w", err)
		}
		return savePublicDirs(ctx, tx, id, wg.PublicDirs)
	})
}

// UpdateWorkgroup replaces the public folders of a workgroup and re-applies
// them to the directories that already exist in it.
func (db *Database) UpdateWorkgroup(wg Workgroup) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT 1
			FROM workgroups
			WHERE id = $1
		`
		var exists int
		if err := tx.QueryRow(ctx, query, wg.ID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("workgroup not found")
			}
			return fmt.Errorf("failed to update workgroup: %w", err)
		}
		if err := savePublicDirs(ctx, tx, wg.ID, wg.PublicDirs); err != nil {
			return err
		}
		return restampDirectories(ctx, tx, wg.ID, wg.PublicDirs)
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
