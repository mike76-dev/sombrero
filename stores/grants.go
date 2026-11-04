package stores

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var ErrNotConnected = errors.New("the workgroup is not connected to the share")

// GrantLevel is a ladder, not a flag set: each level includes the ones below.
type GrantLevel int

const (
	LevelRead GrantLevel = iota
	LevelWrite
	LevelDelete
	LevelAdmin
)

// String names the level the way the API and the UI spell it.
func (l GrantLevel) String() string {
	switch l {
	case LevelRead:
		return "read"
	case LevelWrite:
		return "write"
	case LevelDelete:
		return "delete"
	case LevelAdmin:
		return "admin"
	}
	return fmt.Sprintf("level %d", int(l))
}

// ParseGrantLevel reads a level the way String spells it.
func ParseGrantLevel(s string) (GrantLevel, error) {
	for l := LevelRead; l <= LevelAdmin; l++ {
		if s == l.String() {
			return l, nil
		}
	}
	return 0, fmt.Errorf("unknown grant level %q", s)
}

// Grant shares a folder, and everything under it, with another workgroup.
type Grant struct {
	ShareName string
	Path      string
	Workgroup Workgroup
	Level     GrantLevel
}

// SetGrant grants the folder at the path to the workgroup, or changes the
// level of an existing grant.
func (db *Database) SetGrant(shareName, path string, wg Workgroup, level GrantLevel) error {
	if level < LevelRead || level > LevelAdmin {
		return fmt.Errorf("no such grant level: %d", level)
	}
	path = normalizePath(path)
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		var connected bool
		err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM connections c
				JOIN workgroups w ON w.id = c.workgroup
				WHERE w.uuid = $1 AND c.share_name = $2
			)
		`, wg.UUID[:], shareName).Scan(&connected)
		if err != nil {
			return fmt.Errorf("failed to look for the connection: %w", err)
		}
		if !connected {
			return ErrNotConnected
		}

		const query = `
			INSERT INTO grants (share_name, directory_id, workgroup, level)
			SELECT d.share_name, d.id, w.id, $4
			FROM directories d, workgroups w
			WHERE d.share_name = $1 AND d.full_path = $2 AND w.uuid = $3
			ON CONFLICT (share_name, directory_id, workgroup) DO UPDATE
			SET level = EXCLUDED.level
		`
		res, err := tx.Exec(ctx, query, shareName, path, wg.UUID[:], int16(level))
		if err != nil {
			return fmt.Errorf("failed to store the grant: %w", err)
		}
		if res.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RemoveGrant takes the folder at the path away from the workgroup again.
func (db *Database) RemoveGrant(shareName, path string, wg Workgroup) error {
	path = normalizePath(path)
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			DELETE FROM grants g
			USING directories d, workgroups w
			WHERE g.share_name = d.share_name AND g.directory_id = d.id AND g.workgroup = w.id
			AND d.share_name = $1 AND d.full_path = $2 AND w.uuid = $3
		`
		if _, err := tx.Exec(ctx, query, shareName, path, wg.UUID[:]); err != nil {
			return fmt.Errorf("failed to remove the grant: %w", err)
		}
		return nil
	})
}

// Grants lists who the folder at the path is granted to, this folder alone.
func (db *Database) Grants(shareName, path string) (grants []Grant, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT w.uuid, COALESCE(w.name, ''), g.level
			FROM grants g
			JOIN directories d ON d.share_name = g.share_name AND d.id = g.directory_id
			JOIN workgroups w ON w.id = g.workgroup
			WHERE g.share_name = $1 AND d.full_path = $2
			ORDER BY w.name
		`
		rows, err := tx.Query(ctx, query, shareName, path)
		if err != nil {
			return fmt.Errorf("failed to query the grants: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			g := Grant{ShareName: shareName, Path: path}
			var level int16
			var u []byte
			if err := rows.Scan(&u, &g.Workgroup.Name, &level); err != nil {
				return fmt.Errorf("failed to scan a grant: %w", err)
			}
			copy(g.Workgroup.UUID[:], u)
			g.Level = GrantLevel(level)
			grants = append(grants, g)
		}
		return rows.Err()
	})
	return
}

// WorkgroupGrants lists the folders granted to the workgroup across all shares.
func (db *Database) WorkgroupGrants(wg Workgroup) (grants []Grant, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT g.share_name, d.full_path, g.level
			FROM grants g
			JOIN directories d ON d.share_name = g.share_name AND d.id = g.directory_id
			JOIN workgroups w ON w.id = g.workgroup
			WHERE w.uuid = $1
			ORDER BY g.share_name, d.full_path
		`
		rows, err := tx.Query(ctx, query, wg.UUID[:])
		if err != nil {
			return fmt.Errorf("failed to query the grants: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			g := Grant{Workgroup: wg}
			var level int16
			if err := rows.Scan(&g.ShareName, &g.Path, &level); err != nil {
				return fmt.Errorf("failed to scan a grant: %w", err)
			}
			g.Level = GrantLevel(level)
			grants = append(grants, g)
		}
		return rows.Err()
	})
	return
}

// EffectiveLevel is the highest level the workgroup holds on the path, its own
// grant or an ancestor's: a grant covers the whole subtree under its folder.
func (db *Database) EffectiveLevel(shareName, path string, wg Workgroup) (level GrantLevel, granted bool, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		// The prefix is compared by length, not LIKE, so wildcards in folder names stay literal.
		const query = `
			SELECT MAX(g.level)
			FROM grants g
			JOIN directories d ON d.share_name = g.share_name AND d.id = g.directory_id
			JOIN workgroups w ON w.id = g.workgroup
			WHERE g.share_name = $1 AND w.uuid = $2
			AND (d.full_path = $3 OR left($3, length(d.full_path) + 1) = d.full_path || '/')
		`
		var max *int16
		if err := tx.QueryRow(ctx, query, shareName, wg.UUID[:], path).Scan(&max); err != nil {
			return fmt.Errorf("failed to query the level: %w", err)
		}
		if max != nil {
			level, granted = GrantLevel(*max), true
		}
		return nil
	})
	return
}
