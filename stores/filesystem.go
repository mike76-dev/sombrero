package stores

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
)

var (
	ErrNotFound        = errors.New("object not found")
	ErrNameInvalid     = errors.New("invalid name")
	ErrDirectoryExists = errors.New("directory already exists")
	ErrAccessDenied    = errors.New("access denied")
)

// ObjectMeta represents the metadata of an object in the share.
type ObjectMeta struct {
	Path       string
	Size       uint64
	CreatedAt  time.Time
	ModifiedAt time.Time
	IsDir      bool
}

// listedDirectory finds the directory the account may list at the path, nil
// for the root, reporting whether there is one.
func listedDirectory(ctx context.Context, tx pgx.Tx, acc Account, shareName, path string) (id any, found bool, err error) {
	if path == "/" {
		return nil, true, nil
	}
	const query = `
		WITH caller AS (
			SELECT id, workgroup
			FROM accounts
			WHERE id = $1
		)
		SELECT d.id
		FROM directories d
		JOIN accounts owner ON owner.id = d.account
		CROSS JOIN caller c
		WHERE d.share_name = $2
			AND d.full_path = $3
			AND (d.account = c.id
			OR (d.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
			OR granted_level(d.share_name, c.workgroup, d.full_path) >= 0
			OR owns_ancestor(d.share_name, c.id, d.full_path)
		)
	`
	var dir int64
	if err := tx.QueryRow(ctx, query, acc.ID, shareName, path).Scan(&dir); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to query directory: %v", err)
	}
	return uint64(dir), true, nil
}

// ListObjects lists all objects in the specified directory.
func (db *Database) ListObjects(acc Account, shareName, path string) (objects []ObjectMeta, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			)
			SELECT
				d.full_path,
				0::bigint AS size,
				d.created_at,
				d.modified_at,
				TRUE AS is_dir
			FROM directories d
			CROSS JOIN caller c
			WHERE d.share_name = $1
				AND d.parent_id IS NOT DISTINCT FROM $2
				AND (d.account = c.id
					OR (d.private = FALSE AND EXISTS (
						SELECT 1
						FROM accounts a
						WHERE a.id = d.account
							AND (a.workgroup = c.workgroup OR a.workgroup = anonymous_workgroup())
					))
					OR granted_level(d.share_name, c.workgroup, d.full_path) >= 0
					OR owns_ancestor(d.share_name, c.id, d.full_path)
				)

			UNION ALL

			SELECT
				o.full_path,
				o.size,
				o.created_at,
				o.modified_at,
				FALSE AS is_dir
			FROM objects o
			JOIN accounts owner ON owner.id = o.account
			LEFT JOIN directories od
				ON od.share_name = o.share_name
				AND od.id = o.directory_id
			CROSS JOIN caller c
			WHERE o.share_name = $1
				AND o.directory_id IS NOT DISTINCT FROM $2
				AND o.temporary = FALSE
				AND (
					o.account = c.id
					OR (
						od.private = FALSE
						AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup())
					)
					OR granted_level(o.share_name, c.workgroup, o.full_path) >= 0
					OR owns_ancestor(o.share_name, c.id, o.full_path)
				)

			ORDER BY full_path
		`

		parentID, found, err := listedDirectory(ctx, tx, acc, shareName, path)
		if err != nil || !found {
			return err
		}

		rows, err := tx.Query(ctx, query, shareName, parentID, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to query objects: %v", err)
		}
		defer rows.Close()

		for rows.Next() {
			var obj ObjectMeta
			if err := rows.Scan(&obj.Path, &obj.Size, &obj.CreatedAt, &obj.ModifiedAt, &obj.IsDir); err != nil {
				return fmt.Errorf("failed to scan object: %v", err)
			}
			objects = append(objects, obj)
		}

		if rows.Err() != nil {
			return fmt.Errorf("error iterating over objects: %v", rows.Err())
		}

		return nil
	})

	return
}

// DirectoryEmpty returns true if the specified directory contains no objects.
func (db *Database) DirectoryEmpty(acc Account, shareName, path string) (empty bool, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const dirQuery = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $1
			)
			SELECT d.id
			FROM directories d
			JOIN accounts owner ON owner.id = d.account
			CROSS JOIN caller c
			WHERE d.share_name = $2
				AND d.full_path = $3
				AND (d.account = c.id
				OR (d.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
				OR granted_level(d.share_name, c.workgroup, d.full_path) >= 0
				OR owns_ancestor(d.share_name, c.id, d.full_path)
			)
		`
		const query = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			)
			SELECT NOT (
				EXISTS (
					SELECT 1
					FROM directories d
					JOIN accounts owner ON d.account = owner.id
					CROSS JOIN caller c
					WHERE d.share_name = $1
						AND d.parent_id IS NOT DISTINCT FROM $2
						AND (
							d.account = c.id
							OR (d.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
							OR granted_level(d.share_name, c.workgroup, d.full_path) >= 0
							OR owns_ancestor(d.share_name, c.id, d.full_path)
						)
				)
				OR EXISTS (
					SELECT 1
					FROM objects o
					JOIN accounts owner ON o.account = owner.id
					LEFT JOIN directories od
						ON od.share_name = o.share_name
						AND od.id = o.directory_id
					CROSS JOIN caller c
					WHERE o.share_name = $1
						AND o.directory_id IS NOT DISTINCT FROM $2
						AND (
							o.account = c.id
							OR (
								od.private = FALSE
								AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup())
							)
							OR granted_level(o.share_name, c.workgroup, o.full_path) >= 0
							OR owns_ancestor(o.share_name, c.id, o.full_path)
						)
				)
			) AS is_empty
		`

		var parentID any
		if path == "/" {
			parentID = nil
		} else {
			var parentDir *int64
			if err := tx.QueryRow(ctx, dirQuery, acc.ID, shareName, path).Scan(&parentDir); err != nil {
				if err == pgx.ErrNoRows {
					return nil
				} else {
					return fmt.Errorf("failed to query directory: %v", err)
				}
			}
			parentID = uint64(*parentDir)
		}

		if err := tx.QueryRow(ctx, query, shareName, parentID, acc.ID).Scan(&empty); err != nil {
			return fmt.Errorf("failed to query directory empty status: %v", err)
		}

		return nil
	})

	return
}

// splitPath splits the provided path into the directory and the name.
func splitPath(path string) (directory, name string) {
	if path == "/" {
		return "/", ""
	}

	lastSlash := strings.LastIndex(path, "/")
	if lastSlash == 0 {
		return "/", path[1:]
	}

	return path[:lastSlash], path[lastSlash+1:]
}

// normalizePath normalizes the provided path by replacing backslashes with slashes and ensuring it starts with a slash.
func normalizePath(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	if path != "/" && strings.HasSuffix(path, "/") {
		path = path[:len(path)-1]
	}

	return path
}

// Object retrieves the information about a file or a directory.
func (db *Database) Object(acc Account, shareName, path string) (object ObjectMeta, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			)
			SELECT path, size, created_at, modified_at, is_dir
			FROM (
				SELECT
					d.full_path AS path,
					0::bigint AS size,
					d.created_at,
					d.modified_at,
					TRUE AS is_dir
				FROM directories d
				JOIN accounts owner ON owner.id = d.account
				CROSS JOIN caller c
				WHERE d.share_name = $1
					AND d.full_path = $2
					AND (
						d.account = c.id
						OR (d.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
						OR granted_level(d.share_name, c.workgroup, d.full_path) >= 0
						OR owns_ancestor(d.share_name, c.id, d.full_path)
					)

				UNION ALL

				SELECT
					o.full_path AS path,
					o.size,
					o.created_at,
					o.modified_at,
					FALSE AS is_dir
				FROM objects o
				JOIN accounts owner ON owner.id = o.account
				LEFT JOIN directories od
					ON od.share_name = o.share_name
					AND od.id = o.directory_id
				CROSS JOIN caller c
				WHERE o.share_name = $1
					AND o.full_path = $2
					AND o.temporary = FALSE
					AND (
						o.account = c.id
						OR (
							od.private = FALSE
							AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup())
						)
						OR granted_level(o.share_name, c.workgroup, o.full_path) >= 0
						OR owns_ancestor(o.share_name, c.id, o.full_path)
					)
			) t
			LIMIT 1
		`

		if err := tx.QueryRow(ctx, query, shareName, path, acc.ID).Scan(&object.Path, &object.Size, &object.CreatedAt, &object.ModifiedAt, &object.IsDir); err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound
			} else {
				return fmt.Errorf("failed to query object: %v", err)
			}
		}

		return nil
	})

	return
}

// CurrentAndParent retrieves the information about the given directory and the one above it. The
// root is left empty in both, since it is not a directory of the share but the share itself.
func (db *Database) CurrentAndParent(acc Account, shareName, path string) (currentDir, parentDir ObjectMeta, err error) {
	path = normalizePath(path)
	if path == "/" { // Root directory
		return
	}

	currentDir, err = db.Object(acc, shareName, path)
	if err != nil {
		return
	}

	dir, _ := splitPath(path)
	if dir == "/" {
		return
	}

	parentDir, _ = db.Object(acc, shareName, dir)
	return
}

// CreateDirectory creates a new directory in the database.
// If the directory exists, an error is returned.
// A non-private directory is visible to the whole workgroup of the caller; if it
// is also read-only, only the owner of a file inside it may overwrite or delete
// that file.
func (db *Database) CreateDirectory(acc Account, share string, path string, private, readOnly bool) error {
	path = normalizePath(path)
	parentDir, name := splitPath(path)
	if name == "" {
		return ErrNameInvalid
	}

	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			),
			parent AS (
				SELECT d.id, d.full_path
				FROM directories d
				JOIN accounts owner ON owner.id = d.account
				CROSS JOIN caller c
				WHERE d.share_name = $1
					AND d.full_path = $2
					AND (
						d.account = c.id
						OR (d.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
						OR granted_level(d.share_name, c.workgroup, d.full_path) >= 1
						OR owns_ancestor(d.share_name, c.id, d.full_path)
					)

				UNION ALL

				SELECT NULL::bigint, '/'
				FROM caller
				WHERE $2 = '/'
			)
			INSERT INTO directories (
				share_name,
				parent_id,
				name,
				full_path,
				account,
				workgroup,
				private,
				read_only
			)
			SELECT
				$1,
				p.id,
				$4,
				CASE
					WHEN p.full_path = '/' THEN '/' || $4
					ELSE p.full_path || '/' || $4
				END,
				c.id,
				c.workgroup,
				$5,
				$6
			FROM parent p
			CROSS JOIN caller c
			RETURNING id
		`
		var id uint64
		if err := tx.QueryRow(ctx, query, share, parentDir, acc.ID, name, private, readOnly).Scan(&id); err != nil {
			if strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
				return ErrDirectoryExists
			}
			return fmt.Errorf("failed to create directory: %v", err)
		}
		return nil
	})
}

// RenameFile renames or moves a file. A file renamed over goes, and the slabs
// only it referenced are returned, staged for the caller to unpin.
func (db *Database) RenameFile(acc Account, share string, oldPath, newPath string, force bool) (slabs []types.Hash256, err error) {
	oldPath = normalizePath(oldPath)
	newPath = normalizePath(newPath)
	dir, name := splitPath(newPath)
	if oldPath == "" || name == "" {
		return nil, ErrNameInvalid
	}

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		if force && oldPath != newPath {
			// A file that belongs to another account and sits in a read-only
			// folder must not be renamed over.
			const protectedQuery = `
				SELECT EXISTS (
					SELECT 1
					FROM objects o
					JOIN directories d
						ON d.share_name = o.share_name
						AND d.id = o.directory_id
					WHERE o.share_name = $1
						AND o.full_path = $2
						AND o.temporary = FALSE
						AND o.account <> $3
						AND d.read_only = TRUE
				)
			`
			var protected bool
			if err := tx.QueryRow(ctx, protectedQuery, share, newPath, acc.ID).Scan(&protected); err != nil {
				return fmt.Errorf("failed to check destination file: %w", err)
			}
			if protected {
				return ErrAccessDenied
			}
		}

		// The file renamed over, if there is one the caller may replace. It is
		// deleted in a statement of its own, ahead of the unique index the rename
		// has to pass, or kept as a version where the policy says.
		var doomed []int64
		if force && oldPath != newPath {
			const doomedQuery = `
				WITH caller AS (
					SELECT id, workgroup
					FROM accounts
					WHERE id = $5
				),
				src AS (
					SELECT o.id
					FROM objects o
					LEFT JOIN directories od
						ON od.share_name = o.share_name
						AND od.id = o.directory_id
					CROSS JOIN caller c
					WHERE o.share_name = $1
						AND o.full_path = $2
						AND o.temporary = FALSE
						AND (o.account = c.id
							OR (od.read_only = FALSE AND (
								granted_level(o.share_name, c.workgroup, o.full_path) >= 2
								OR owns_ancestor(o.share_name, c.id, o.full_path)
							))
						)
				),
				dst_parent AS (
					SELECT d.id, d.full_path, d.read_only
					FROM directories d
					JOIN accounts owner ON owner.id = d.account
					CROSS JOIN caller c
					WHERE d.share_name = $1
						AND d.full_path = $3
						AND (
							d.account = c.id
							OR (d.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
							OR granted_level(d.share_name, c.workgroup, d.full_path) >= 1
							OR owns_ancestor(d.share_name, c.id, d.full_path)
						)

					UNION ALL

					SELECT NULL::bigint, '/', FALSE
					FROM caller
					WHERE $3 = '/'
				),
				target AS (
					SELECT
						p.read_only AS new_parent_read_only,
						$4::text AS new_path
					FROM src s
					JOIN dst_parent p ON TRUE
					WHERE NOT EXISTS (
						SELECT 1
						FROM directories d
						WHERE d.share_name = $1
							AND d.full_path = $4
					)
				)
				SELECT o.id
				FROM objects o
				JOIN target t ON o.full_path = t.new_path
				CROSS JOIN caller c
				WHERE o.share_name = $1
					AND o.temporary = FALSE
					AND (o.account = c.id OR t.new_parent_read_only = FALSE)
			`

			rows, err := tx.Query(ctx, doomedQuery, share, oldPath, dir, newPath, acc.ID)
			if err != nil {
				return fmt.Errorf("failed to find the file renamed over: %w", err)
			}
			if doomed, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
				return fmt.Errorf("failed to read the file renamed over: %w", err)
			}
		}

		var bids []uint64
		var keys [][]byte
		var kept bool
		if len(doomed) > 0 {
			keep, err := db.keepsVersion(ctx, tx, share, newPath, uint64(acc.ID))
			if err != nil {
				return err
			}
			if keep {
				if err := recordVersions(ctx, tx, doomed, uint64(acc.ID)); err != nil {
					return err
				}
				kept = true
			} else {
				const collectDoomed = `
					SELECT DISTINCT m.buffer_id, m.slab_key
					FROM metadata m
					WHERE m.object_id = ANY($1::BIGINT[])
				`

				rows, err := tx.Query(ctx, collectDoomed, doomed)
				if err != nil {
					return fmt.Errorf("failed to collect the storage of the file renamed over: %w", err)
				}
				if bids, keys, err = collectStorage(rows); err != nil {
					return err
				}

				const deleteDoomed = `
					DELETE FROM objects
					WHERE id = ANY($1::BIGINT[])
				`

				if _, err := tx.Exec(ctx, deleteDoomed, doomed); err != nil {
					return fmt.Errorf("failed to delete existing destination file: %w", err)
				}
			}
		}

		const renameQuery = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $6
			),
			src AS (
				SELECT o.id, o.full_path
				FROM objects o
				LEFT JOIN directories od
					ON od.share_name = o.share_name
					AND od.id = o.directory_id
				CROSS JOIN caller c
				WHERE o.share_name = $1
					AND o.full_path = $2
					AND o.temporary = FALSE
					AND (o.account = c.id
						OR (od.read_only = FALSE AND (
							granted_level(o.share_name, c.workgroup, o.full_path) >= 2
							OR owns_ancestor(o.share_name, c.id, o.full_path)
						))
					)
			),
			dst_parent AS (
				SELECT d.id, d.full_path
				FROM directories d
				JOIN accounts owner ON owner.id = d.account
				CROSS JOIN caller c
				WHERE d.share_name = $1
					AND d.full_path = $3
					AND (
						d.account = c.id
						OR (d.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
						OR granted_level(d.share_name, c.workgroup, d.full_path) >= 1
						OR owns_ancestor(d.share_name, c.id, d.full_path)
					)

				UNION ALL

				SELECT NULL::bigint, '/'
				FROM caller
				WHERE $3 = '/'
			),
			target AS (
				SELECT
					s.id AS src_id,
					p.id AS new_parent_id,
					$4::text AS new_name,
					$5::text AS new_path
				FROM src s
				JOIN dst_parent p ON TRUE
				WHERE $5 <> $2
					AND NOT EXISTS (
						SELECT 1
						FROM directories d
						WHERE d.share_name = $1
							AND d.full_path = $5
					)
			)
			UPDATE objects o
			SET
				directory_id = t.new_parent_id,
				name = t.new_name,
				full_path = t.new_path
			FROM target t
			WHERE o.id = t.src_id
			RETURNING o.id
		`

		var id uint64
		if err := tx.QueryRow(ctx, renameQuery, share, oldPath, dir, name, newPath, acc.ID).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("failed to rename file: %v", err)
		}

		// The versions of the file follow it to where it now is.
		const moveVersions = `
			UPDATE objects o
			SET directory_id = f.directory_id,
				name = f.name,
				full_path = f.full_path
			FROM objects f, versions v
			WHERE f.id = $3
				AND v.object_id = o.id
				AND o.share_name = $1
				AND o.full_path = $2
		`

		if _, err := tx.Exec(ctx, moveVersions, share, oldPath, id); err != nil {
			return fmt.Errorf("failed to move the versions: %w", err)
		}

		// Where the file came from is noted: deleted soon, with the name taken
		// by another file, it will have been the save of an editor that swaps.
		if db.versioning.Keep {
			const displace = `
				INSERT INTO displaced (object_id, path)
				VALUES ($1, $2)
				ON CONFLICT (object_id) DO UPDATE
				SET path = EXCLUDED.path, at = NOW()
			`

			if _, err := tx.Exec(ctx, displace, id, oldPath); err != nil {
				return fmt.Errorf("failed to note the file's old name: %w", err)
			}
		}

		if kept {
			if slabs, err = db.pruneVersions(ctx, tx, share, newPath); err != nil {
				return err
			}
		}

		for _, bid := range bids {
			if _, err := tx.Exec(ctx, `
				DELETE FROM buffers b
				WHERE b.id = $1
					AND NOT EXISTS (
						SELECT 1
						FROM metadata m
						WHERE m.buffer_id = b.id
					)
			`, bid); err != nil {
				return fmt.Errorf("failed to delete orphaned buffer: %v", err)
			}
		}

		// The file that was renamed over is gone, so what only it referenced is
		// staged to be unpinned by the caller's workgroup connection.
		if len(keys) == 0 {
			return nil
		}
		var workgroup int
		if err := tx.QueryRow(ctx, `SELECT workgroup FROM accounts WHERE id = $1`, acc.ID).Scan(&workgroup); err != nil {
			return fmt.Errorf("failed to resolve workgroup: %w", err)
		}
		released, err := unreferencedSlabs(ctx, tx, share, workgroup, keys)
		if err != nil {
			return err
		}
		slabs = append(slabs, released...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return slabs, nil
}

// RenameDirectory renames or moves a directory. A directory renamed over goes
// with everything in it, and the slabs only its files referenced are returned,
// staged for the caller to unpin.
func (db *Database) RenameDirectory(acc Account, share string, oldPath, newPath string, force bool) (slabs []types.Hash256, err error) {
	oldPath = normalizePath(oldPath)
	newPath = normalizePath(newPath)
	dir, name := splitPath(newPath)
	if oldPath == "" || name == "" {
		return nil, ErrNameInvalid
	}

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		if force && oldPath != newPath {
			// A read-only directory that belongs to another account must not
			// be renamed over.
			const protectedQuery = `
				SELECT EXISTS (
					SELECT 1
					FROM directories d
					WHERE d.share_name = $1
						AND d.full_path = $2
						AND d.account <> $3
						AND d.read_only = TRUE
				)
			`
			var protected bool
			if err := tx.QueryRow(ctx, protectedQuery, share, newPath, acc.ID).Scan(&protected); err != nil {
				return fmt.Errorf("failed to check destination directory: %w", err)
			}
			if protected {
				return ErrAccessDenied
			}
		}

		// What the directory being renamed over holds, folders down, noted before
		// the cascade takes it: the cascade knows nothing of buffers or slabs.
		var bids []uint64
		var keys [][]byte
		if force && oldPath != newPath {
			const collectDoomed = `
				SELECT DISTINCT m.buffer_id, m.slab_key
				FROM metadata m
				JOIN objects o ON o.id = m.object_id
				WHERE o.share_name = $1
					AND starts_with(o.full_path, $2 || '/')
					AND EXISTS (
						SELECT 1 FROM directories d
						WHERE d.share_name = $1 AND d.full_path = $2
					)
			`
			rows, err := tx.Query(ctx, collectDoomed, share, newPath)
			if err != nil {
				return fmt.Errorf("failed to collect the storage of the directory renamed over: %w", err)
			}
			if bids, keys, err = collectStorage(rows); err != nil {
				return err
			}

			// The directory renamed over goes in a statement of its own: deleted
			// in a CTE of the rename, it would still be there to the unique index
			// the rename has to pass, since a statement cannot see its own changes.
			// A rename that then finds nothing to rename rolls this back with it.
			const deleteExisting = `
				DELETE FROM directories d
				USING accounts c
				WHERE c.id = $3
					AND d.share_name = $1
					AND d.full_path = $2
					AND (d.account = c.id OR d.read_only = FALSE)
			`
			if _, err := tx.Exec(ctx, deleteExisting, share, newPath, acc.ID); err != nil {
				return fmt.Errorf("failed to delete the directory renamed over: %w", err)
			}
		}

		// The subtree is matched with starts_with, not LIKE: a directory named
		// with a _ or a % would make LIKE reach into its siblings.
		const query = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $6
			),
			src AS (
				SELECT d.id, d.full_path
				FROM directories d
				JOIN accounts owner ON owner.id = d.account
				CROSS JOIN caller c
				WHERE d.share_name = $1
					AND d.full_path = $2
					AND (
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
					)
			),
			dst_parent AS (
				SELECT d.id, d.full_path
				FROM directories d
				JOIN accounts owner ON owner.id = d.account
				CROSS JOIN caller c
				WHERE d.share_name = $1
					AND d.full_path = $3
					AND (
						d.account = c.id
						OR (d.private = FALSE AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup()))
						OR granted_level(d.share_name, c.workgroup, d.full_path) >= 1
						OR owns_ancestor(d.share_name, c.id, d.full_path)
					)

				UNION ALL

				SELECT NULL::bigint, '/'
				FROM caller
				WHERE $3 = '/'
			),
			target AS (
				SELECT
					s.id AS src_id,
					s.full_path AS old_path,
					p.id AS new_parent_id,
					$4::text AS new_name,
					$5::text AS new_path
				FROM src s
				JOIN dst_parent p ON TRUE
				WHERE $5 <> s.full_path
					AND NOT starts_with($5::text, s.full_path || '/')
			),
			-- The permission to rename is decided on the directory itself in
			-- src; everything underneath it has to follow the rename
			-- unconditionally, or the paths of the entries the caller may not
			-- touch would go stale.
			update_subdirs AS (
				UPDATE directories d
				SET
					full_path = t.new_path || substring(d.full_path FROM length(t.old_path) + 1),
					modified_at = NOW()
				FROM target t
				WHERE d.share_name = $1
					AND d.id <> t.src_id
					AND starts_with(d.full_path, t.old_path || '/')
			),
			update_files AS (
				UPDATE objects o
				SET
					full_path = t.new_path || substring(o.full_path FROM length(t.old_path) + 1)
				FROM target t
				WHERE o.share_name = $1
					AND starts_with(o.full_path, t.old_path || '/')
			)
			UPDATE directories d
			SET
				parent_id = t.new_parent_id,
				name = t.new_name,
				full_path = t.new_path,
				modified_at = NOW()
			FROM target t
			WHERE d.id = t.src_id
		`

		tag, err := tx.Exec(ctx, query, share, oldPath, dir, name, newPath, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to rename directory: %v", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}

		for _, bid := range bids {
			if _, err := tx.Exec(ctx, `
				DELETE FROM buffers b
				WHERE b.id = $1
					AND NOT EXISTS (
						SELECT 1
						FROM metadata m
						WHERE m.buffer_id = b.id
					)
			`, bid); err != nil {
				return fmt.Errorf("failed to delete orphaned buffer: %w", err)
			}
		}

		// The directory that was renamed over is gone with its files, so what
		// only they referenced is staged to be unpinned by the caller's workgroup
		// connection.
		if len(keys) == 0 {
			return nil
		}
		var workgroup int
		if err := tx.QueryRow(ctx, `SELECT workgroup FROM accounts WHERE id = $1`, acc.ID).Scan(&workgroup); err != nil {
			return fmt.Errorf("failed to resolve workgroup: %w", err)
		}
		slabs, err = unreferencedSlabs(ctx, tx, share, workgroup, keys)
		return err
	})
	if err != nil {
		return nil, err
	}

	return slabs, nil
}

// DeleteFile deletes a file. It returns the keys of the slabs that no other
// file references any more, so that the caller can unpin them.
//
// A file that exists only as an upload in flight is deleted along with the
// upload, which is what a client asking for a half-written file to go means. An
// upload overwriting a file that is already there is left alone: the file being
// deleted is the one that is on the share, and the writer's own close decides
// what happens to theirs.
func (db *Database) DeleteFile(acc Account, share string, path string) (slabs []types.Hash256, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		// A file renamed away from a name another file has taken since, and
		// deleted soon after, is an editor's swap save: it replaced that file.
		origin, err := displacedOrigin(ctx, tx, share, path)
		if err != nil {
			return err
		}
		retain := db.versioning.Keep
		if origin != "" {
			if err := moveVersionsTo(ctx, tx, share, path, origin); err != nil {
				return err
			}
			keep, err := db.keepsVersion(ctx, tx, share, origin, uint64(acc.ID))
			if err != nil {
				return err
			}
			if keep {
				slabs, err = db.absorbFile(ctx, tx, share, path, origin, acc)
				return err
			}
			// Within the session, the old file is a save's leftovers and goes outright.
			retain = false
		}

		// A share that keeps versions deletes a file by emptying it: the contents
		// become a version, and the versions stay for the folder to show it by.
		if retain {
			slabs, err = db.emptyFile(ctx, tx, share, path, acc, true)
			if !errors.Is(err, ErrNotFound) {
				return err
			}
		}

		const collectQuery = deletableFile + `
			SELECT DISTINCT m.buffer_id, m.slab_key
			FROM metadata m
			JOIN target t ON m.object_id = t.id
		`

		rows, err := tx.Query(ctx, collectQuery, share, path, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to collect file storage: %w", err)
		}

		bids, keys, err := collectStorage(rows)
		if err != nil {
			return err
		}

		const deleteQuery = deletableFile + `,
			deleted_objects AS (
				DELETE FROM objects o
				USING target t
				WHERE o.id = t.id
				RETURNING o.id
			)
			SELECT COUNT(*)
			FROM deleted_objects
		`

		var n int64
		err = tx.QueryRow(ctx, deleteQuery, share, path, acc.ID).Scan(&n)
		if err != nil {
			return fmt.Errorf("failed to delete file: %v", err)
		}
		if n == 0 {
			return ErrNotFound
		}

		// The versions of the file go with it where none are kept.
		var released []types.Hash256
		if !db.versioning.Keep {
			if released, err = dropVersionsAt(ctx, tx, share, path, 0); err != nil {
				return err
			}
		}

		for _, bid := range bids {
			if _, err := tx.Exec(ctx, `
				DELETE FROM buffers b
				WHERE b.id = $1
					AND NOT EXISTS (
						SELECT 1
						FROM metadata m
						WHERE m.buffer_id = b.id
					)
			`, bid); err != nil {
				return fmt.Errorf("failed to delete orphaned buffer: %w", err)
			}
		}

		// The metadata is gone by now, so any slab that is still referenced
		// belongs to another file and must stay pinned. The rest is staged to
		// be unpinned by the caller's workgroup connection, which is the one
		// whose app account pinned it.
		var workgroup int
		if err := tx.QueryRow(ctx, `SELECT workgroup FROM accounts WHERE id = $1`, acc.ID).Scan(&workgroup); err != nil {
			return fmt.Errorf("failed to resolve workgroup: %w", err)
		}

		if slabs, err = unreferencedSlabs(ctx, tx, share, workgroup, keys); err != nil {
			return err
		}
		slabs = append(slabs, released...)
		return nil
	})

	if err != nil {
		return nil, err
	}
	return
}

// deletableFile opens the queries of DeleteFile: the CTE target is the file at
// the path ($1, $2) the caller ($3) may delete, an upload in flight included
// where no visible file is there, and a version never.
const deletableFile = `
	WITH caller AS (
		SELECT id, workgroup
		FROM accounts
		WHERE id = $3
	),
	target AS (
		SELECT o.id
		FROM objects o
		JOIN accounts owner ON owner.id = o.account
		LEFT JOIN directories od
			ON od.share_name = o.share_name
			AND od.id = o.directory_id
		CROSS JOIN caller c
		WHERE o.share_name = $1
			AND o.full_path = $2
			AND (
				o.temporary = FALSE
				OR (
					NOT EXISTS (
						SELECT 1
						FROM versions kept
						WHERE kept.object_id = o.id
					)
					AND NOT EXISTS (
						SELECT 1
						FROM objects v
						WHERE v.share_name = $1
							AND v.full_path = $2
							AND v.temporary = FALSE
					)
				)
			)
			AND (
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
			)
	)
`

// displacedWindow is how soon after being renamed away a file has to be
// deleted for the deletion to count as replacing the file now under its name.
const displacedWindow = 5 * time.Minute

// displacedOrigin returns the name the file at the path was renamed away from
// within the window, if another visible file has it now, and "" otherwise.
func displacedOrigin(ctx context.Context, tx pgx.Tx, share, path string) (string, error) {
	const query = `
		SELECT d.path
		FROM displaced d
		JOIN objects o ON o.id = d.object_id
		WHERE o.share_name = $1
			AND o.full_path = $2
			AND o.temporary = FALSE
			AND d.at > NOW() - make_interval(secs => $3)
			AND EXISTS (
				SELECT 1
				FROM objects f
				WHERE f.share_name = $1
					AND f.full_path = d.path
					AND f.temporary = FALSE
			)
	`

	var origin string
	err := tx.QueryRow(ctx, query, share, path, displacedWindow.Seconds()).Scan(&origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("failed to look up the file's old name: %w", err)
	}
	return origin, nil
}

// moveVersionsTo moves the versions at the path under the visible file at the
// destination.
func moveVersionsTo(ctx context.Context, tx pgx.Tx, share, path, dest string) error {
	const query = `
		UPDATE objects o
		SET directory_id = f.directory_id,
			name = f.name,
			full_path = f.full_path
		FROM objects f, versions v
		WHERE f.share_name = $1
			AND f.full_path = $3
			AND f.temporary = FALSE
			AND v.object_id = o.id
			AND o.share_name = $1
			AND o.full_path = $2
	`

	if _, err := tx.Exec(ctx, query, share, path, dest); err != nil {
		return fmt.Errorf("failed to move the versions: %w", err)
	}
	return nil
}

// absorbFile makes the file at the path, which the caller deletes, a version
// of the file at the origin, and returns the slabs the pruning releases.
func (db *Database) absorbFile(ctx context.Context, tx pgx.Tx, share, path, origin string, acc Account) ([]types.Hash256, error) {
	const targets = deletableFile + `
		SELECT t.id
		FROM target t
	`

	rows, err := tx.Query(ctx, targets, share, path, acc.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to find the file: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("failed to read the file: %w", err)
	}
	if len(ids) == 0 {
		return nil, ErrNotFound
	}

	if err := recordVersions(ctx, tx, ids, uint64(acc.ID)); err != nil {
		return nil, err
	}

	const move = `
		UPDATE objects o
		SET directory_id = f.directory_id,
			name = f.name,
			full_path = f.full_path
		FROM objects f
		WHERE f.share_name = $1
			AND f.full_path = $2
			AND f.temporary = FALSE
			AND o.id = ANY($3::BIGINT[])
	`

	if _, err := tx.Exec(ctx, move, share, origin, ids); err != nil {
		return nil, fmt.Errorf("failed to move the version under its name: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM displaced WHERE object_id = ANY($1::BIGINT[])`, ids); err != nil {
		return nil, fmt.Errorf("failed to forget the file's old name: %w", err)
	}
	return db.pruneVersions(ctx, tx, share, origin)
}

// DeleteDirectory deletes a directory and all its contents. It returns the keys
// of the slabs that no other file references any more, so that the caller can
// unpin them.
func (db *Database) DeleteDirectory(acc Account, share string, path string) (slabs []types.Hash256, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		// The subtree is matched with starts_with, not LIKE: a directory named
		// with a _ or a % would make LIKE reach into its siblings.
		const collectQuery = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			),
			src AS (
				SELECT d.id, d.full_path
				FROM directories d
				JOIN accounts owner ON owner.id = d.account
				CROSS JOIN caller c
				WHERE d.share_name = $1
					AND d.full_path = $2
					AND (
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
					)
			),
			-- A file still being uploaded into the directory goes with it, and its
			-- storage is collected here: the object would be taken by the cascade
			-- of the directory alone, leaving its buffer and its slabs paid for
			-- and referenced by nobody.
			target_objects AS (
				SELECT o.id
				FROM objects o
				JOIN src s ON TRUE
				WHERE o.share_name = $1
					AND starts_with(o.full_path, s.full_path || '/')
			)
			SELECT DISTINCT m.buffer_id, m.slab_key
			FROM metadata m
			JOIN target_objects t ON m.object_id = t.id
		`

		rows, err := tx.Query(ctx, collectQuery, share, path, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to collect directory storage: %w", err)
		}

		bids, keys, err := collectStorage(rows)
		if err != nil {
			return err
		}

		const deleteQuery = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			),
			src AS (
				SELECT d.id, d.full_path
				FROM directories d
				JOIN accounts owner ON owner.id = d.account
				CROSS JOIN caller c
				WHERE d.share_name = $1
					AND d.full_path = $2
					AND (
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
					)
			),
			delete_files AS (
				DELETE FROM objects o
				USING src s
				WHERE o.share_name = $1
					AND starts_with(o.full_path, s.full_path || '/')
				RETURNING o.id
			),
			delete_dirs AS (
				DELETE FROM directories d
				USING src s
				WHERE d.share_name = $1
					AND starts_with(d.full_path, s.full_path || '/')
				RETURNING d.id
			),
			delete_root AS (
				DELETE FROM directories d
				USING src s
				WHERE d.id = s.id
				RETURNING d.id
			)
			SELECT COUNT(*)
			FROM delete_root
		`

		var n int64
		err = tx.QueryRow(ctx, deleteQuery, share, path, acc.ID).Scan(&n)
		if err != nil {
			return fmt.Errorf("failed to delete directory: %v", err)
		}
		if n == 0 {
			return ErrNotFound
		}

		for _, bid := range bids {
			if _, err := tx.Exec(ctx, `
				DELETE FROM buffers b
				WHERE b.id = $1
					AND NOT EXISTS (
						SELECT 1
						FROM metadata m
						WHERE m.buffer_id = b.id
					)
			`, bid); err != nil {
				return fmt.Errorf("failed to delete orphaned buffer: %w", err)
			}
		}

		// The metadata is gone by now, so any slab that is still referenced
		// belongs to another file and must stay pinned. The rest is staged to
		// be unpinned by the caller's workgroup connection, which is the one
		// whose app account pinned it.
		var workgroup int
		if err := tx.QueryRow(ctx, `SELECT workgroup FROM accounts WHERE id = $1`, acc.ID).Scan(&workgroup); err != nil {
			return fmt.Errorf("failed to resolve workgroup: %w", err)
		}

		slabs, err = unreferencedSlabs(ctx, tx, share, workgroup, keys)
		return err
	})

	if err != nil {
		return nil, err
	}
	return
}
