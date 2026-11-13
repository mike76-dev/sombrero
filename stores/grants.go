package stores

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
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

// ShareGrants lists every grant on the share, for the share's settings page.
func (db *Database) ShareGrants(shareName string) (grants []Grant, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT d.full_path, w.uuid, COALESCE(w.name, ''), g.level
			FROM grants g
			JOIN directories d ON d.share_name = g.share_name AND d.id = g.directory_id
			JOIN workgroups w ON w.id = g.workgroup
			WHERE g.share_name = $1
			ORDER BY d.full_path, w.name
		`
		rows, err := tx.Query(ctx, query, shareName)
		if err != nil {
			return fmt.Errorf("failed to query the grants: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			g := Grant{ShareName: shareName}
			var level int16
			var u []byte
			if err := rows.Scan(&g.Path, &u, &g.Workgroup.Name, &level); err != nil {
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

// PathSecurity is what a security descriptor is built from: who owns the path,
// and which workgroups hold grants on it, folded over the ancestors.
type PathSecurity struct {
	OwnerUser      string
	OwnerWorkgroup uuid.UUID
	Grants         []Grant
}

// PathSecurity reports the owner of the path and the workgroups it is granted
// to, each with the highest level it holds here.
func (db *Database) PathSecurity(shareName, path string) (ps PathSecurity, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const ownerQuery = `
			SELECT a.account_name, w.uuid
			FROM (
				SELECT d.account FROM directories d
				WHERE d.share_name = $1 AND d.full_path = $2
				UNION ALL
				SELECT o.account FROM objects o
				WHERE o.share_name = $1 AND o.full_path = $2 AND o.temporary = FALSE
			) t
			JOIN accounts a ON a.id = t.account
			JOIN workgroups w ON w.id = a.workgroup
			LIMIT 1
		`
		var u []byte
		if err := tx.QueryRow(ctx, ownerQuery, shareName, path).Scan(&ps.OwnerUser, &u); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("failed to query the owner: %w", err)
		}
		copy(ps.OwnerWorkgroup[:], u)

		const grantsQuery = `
			SELECT w.uuid, COALESCE(w.name, ''), MAX(g.level)
			FROM grants g
			JOIN directories d ON d.share_name = g.share_name AND d.id = g.directory_id
			JOIN workgroups w ON w.id = g.workgroup
			WHERE g.share_name = $1
			AND (d.full_path = $2 OR starts_with($2, d.full_path || '/'))
			GROUP BY w.uuid, w.name
			ORDER BY w.name
		`
		rows, err := tx.Query(ctx, grantsQuery, shareName, path)
		if err != nil {
			return fmt.Errorf("failed to query the grants: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			g := Grant{ShareName: shareName, Path: path}
			var level int16
			var wu []byte
			if err := rows.Scan(&wu, &g.Workgroup.Name, &level); err != nil {
				return fmt.Errorf("failed to scan a grant: %w", err)
			}
			copy(g.Workgroup.UUID[:], wu)
			g.Level = GrantLevel(level)
			ps.Grants = append(ps.Grants, g)
		}
		return rows.Err()
	})
	return
}

// PathAccess is what the account may do at one path. For a path that holds
// nothing yet, Write is the right to put something of the account's own there.
type PathAccess struct {
	Write  bool
	Delete bool
}

// PathAccess reports what the account may do at the path, mirroring the
// predicates the write and delete queries enforce. A path the account cannot
// see, and whose parent it cannot see either, is ErrNotFound.
func (db *Database) PathAccess(acc Account, shareName, path string) (pa PathAccess, err error) {
	path = normalizePath(path)
	if path == "/" {
		return PathAccess{Write: true}, nil
	}

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const objectQuery = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			)
			SELECT
				COALESCE(
					o.account = c.id
					OR (od.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
					OR granted_level(o.share_name, c.workgroup, o.full_path) >= 0
					OR owns_ancestor(o.share_name, c.id, o.full_path)
				, FALSE) AS visible,
				COALESCE(
					COALESCE(
						o.directory_id IS NULL
						OR od.account = c.id
						OR (od.private = FALSE AND (powner.workgroup = c.workgroup OR powner.workgroup = anonymous_workgroup()))
						OR granted_level(o.share_name, c.workgroup, o.full_path) >= 1
						OR owns_ancestor(o.share_name, c.id, o.full_path)
					, FALSE)
					AND (od.read_only IS NOT TRUE OR o.account = c.id)
				, FALSE) AS can_write,
				COALESCE(
					o.account = c.id
					OR (
						od.private = FALSE
						AND od.read_only = FALSE
						AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup())
					)
					OR (od.read_only = FALSE AND (
						granted_level(o.share_name, c.workgroup, o.full_path) >= 2
						OR owns_ancestor(o.share_name, c.id, o.full_path)
					))
				, FALSE) AS can_delete
			FROM objects o
			JOIN accounts owner ON owner.id = o.account
			LEFT JOIN directories od
				ON od.share_name = o.share_name
				AND od.id = o.directory_id
			LEFT JOIN accounts powner ON powner.id = od.account
			CROSS JOIN caller c
			WHERE o.share_name = $1
				AND o.full_path = $2
				AND o.temporary = FALSE
		`

		var visible, canWrite, canDelete bool
		err := tx.QueryRow(ctx, objectQuery, shareName, path, acc.ID).Scan(&visible, &canWrite, &canDelete)
		if err == nil {
			if !visible {
				return ErrNotFound
			}
			pa = PathAccess{Write: canWrite, Delete: canDelete}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("failed to query the object: %w", err)
		}

		const directoryQuery = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			)
			SELECT
				COALESCE(
					d.account = c.id
					OR (d.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
					OR granted_level(d.share_name, c.workgroup, d.full_path) >= 0
					OR owns_ancestor(d.share_name, c.id, d.full_path)
				, FALSE) AS visible,
				COALESCE(
					d.account = c.id
					OR (d.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
					OR granted_level(d.share_name, c.workgroup, d.full_path) >= 1
					OR owns_ancestor(d.share_name, c.id, d.full_path)
				, FALSE) AS can_write,
				COALESCE(
					d.account = c.id
					OR (
						d.private = FALSE
						AND d.read_only = FALSE
						AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup())
					)
					OR (d.read_only = FALSE AND (
						granted_level(d.share_name, c.workgroup, d.full_path) >= 2
						OR owns_ancestor(d.share_name, c.id, d.full_path)
					))
				, FALSE) AS can_delete
			FROM directories d
			JOIN accounts owner ON owner.id = d.account
			CROSS JOIN caller c
			WHERE d.share_name = $1
				AND d.full_path = $2
		`

		err = tx.QueryRow(ctx, directoryQuery, shareName, path, acc.ID).Scan(&visible, &canWrite, &canDelete)
		if err == nil {
			if !visible {
				return ErrNotFound
			}
			pa = PathAccess{Write: canWrite, Delete: canDelete}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("failed to query the directory: %w", err)
		}

		// Nothing is at the path, so the parent decides, and what would be made
		// there would be the caller's own to delete.
		parent, _ := splitPath(path)
		if parent == "/" {
			pa = PathAccess{Write: true, Delete: true}
			return nil
		}

		err = tx.QueryRow(ctx, directoryQuery, shareName, parent, acc.ID).Scan(&visible, &canWrite, &canDelete)
		if err == nil {
			if !visible {
				return ErrNotFound
			}
			pa = PathAccess{Write: canWrite, Delete: canWrite}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("failed to query the parent: %w", err)
		}

		// The parent is gone too, and only the versions of a deleted file
		// can be at the path: they answer for themselves, as their folder
		// no longer can.
		const versionsQuery = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			)
			SELECT
				COALESCE(bool_or(
					o.account = c.id
					OR granted_level(o.share_name, c.workgroup, o.full_path) >= 0
					OR owns_ancestor(o.share_name, c.id, o.full_path)
				), FALSE),
				COALESCE(bool_or(
					o.account = c.id
					OR granted_level(o.share_name, c.workgroup, o.full_path) >= 1
					OR owns_ancestor(o.share_name, c.id, o.full_path)
				), FALSE),
				COALESCE(bool_or(
					o.account = c.id
					OR granted_level(o.share_name, c.workgroup, o.full_path) >= 2
					OR owns_ancestor(o.share_name, c.id, o.full_path)
				), FALSE)
			FROM versions v
			JOIN objects o ON o.id = v.object_id
			CROSS JOIN caller c
			WHERE o.share_name = $1
				AND o.full_path = $2
		`

		if err := tx.QueryRow(ctx, versionsQuery, shareName, path, acc.ID).Scan(&visible, &canWrite, &canDelete); err != nil {
			return fmt.Errorf("failed to query the versions: %w", err)
		}
		if !visible {
			return ErrNotFound
		}
		pa = PathAccess{Write: canWrite, Delete: canDelete}
		return nil
	})
	return
}

// EffectiveLevel is the highest level the workgroup holds on the path, its own
// grant or an ancestor's: a grant covers the whole subtree under its folder.
func (db *Database) EffectiveLevel(shareName, path string, wg Workgroup) (level GrantLevel, granted bool, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT granted_level($1, w.id, $3)
			FROM workgroups w
			WHERE w.uuid = $2
		`
		var max *int16
		err := tx.QueryRow(ctx, query, shareName, wg.UUID[:], path).Scan(&max)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to query the level: %w", err)
		}
		if max != nil {
			level, granted = GrantLevel(*max), true
		}
		return nil
	})
	return
}
