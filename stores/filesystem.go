package stores

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrNotFound        = errors.New("object not found")
	ErrNameInvalid     = errors.New("invalid name")
	ErrDirectoryExists = errors.New("directory already exists")
)

// ObjectMeta represents the metadata of an object in the share.
type ObjectMeta struct {
	Path       string
	Size       uint64
	CreatedAt  time.Time
	ModifiedAt time.Time
	IsDir      bool
}

// ListObjects lists all objects in the specified directory.
func (db *Database) ListObjects(acc Account, shareName, path string) (objects []ObjectMeta, err error) {
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
				OR (d.private = FALSE AND owner.workgroup = c.workgroup)
			)
		`
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
							AND a.workgroup = c.workgroup
					))
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
						AND owner.workgroup = c.workgroup
					)
				)

			ORDER BY full_path
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
				OR (d.private = FALSE AND owner.workgroup = c.workgroup)
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
							OR (d.private = FALSE AND owner.workgroup = c.workgroup)
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
								AND owner.workgroup = c.workgroup
							)
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
						OR (d.private = FALSE AND owner.workgroup = c.workgroup)
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
							AND owner.workgroup = c.workgroup
						)
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

// CurrentAndParent retrieves the information about the current and the parent directories where the file is located.
func (db *Database) CurrentAndParent(acc Account, shareName, path string) (currentDir, parentDir ObjectMeta, err error) {
	path = normalizePath(path)
	dir, _ := splitPath(path)
	if dir == "/" { // Root directory
		return
	}

	currentDir, err = db.Object(acc, shareName, dir)
	if err != nil {
		return
	}

	dir, _ = splitPath(dir)
	if dir == "/" {
		return
	}

	parentDir, _ = db.Object(acc, shareName, dir)
	return
}

// CreateDirectory creates a new directory in the database.
// If the directory exists, an error is returned.
func (db *Database) CreateDirectory(acc Account, share string, path string, private bool) error {
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
						OR (d.private = FALSE AND owner.workgroup = c.workgroup)
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
				private
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
				$5
			FROM parent p
			CROSS JOIN caller c
			RETURNING id
		`
		var id uint64
		if err := tx.QueryRow(ctx, query, share, parentDir, acc.ID, name, private).Scan(&id); err != nil {
			if strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
				return ErrDirectoryExists
			}
			return fmt.Errorf("failed to create directory: %v", err)
		}
		return nil
	})
}

// RenameFile renames or moves a file.
func (db *Database) RenameFile(acc Account, share string, oldPath, newPath string, force bool) error {
	oldPath = normalizePath(oldPath)
	newPath = normalizePath(newPath)
	dir, name := splitPath(newPath)
	if oldPath == "" || name == "" {
		return ErrNameInvalid
	}

	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const collectQuery = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $6
			),
			src AS (
				SELECT o.id
				FROM objects o
				CROSS JOIN caller c
				WHERE o.share_name = $1
					AND o.full_path = $2
					AND o.temporary = FALSE
					AND o.account = c.id
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
						OR (d.private = FALSE AND owner.workgroup = c.workgroup)
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
			),
			check_no_dir_conflict AS (
				SELECT 1
				FROM target t
				WHERE NOT EXISTS (
					SELECT 1
					FROM directories d
					WHERE d.share_name = $1
						AND d.full_path = t.new_path
				)
			),
			doomed_target AS (
				SELECT o.id
				FROM objects o
				JOIN target t ON o.full_path = t.new_path
				JOIN check_no_dir_conflict c ON TRUE
				WHERE o.share_name = $1
					AND o.temporary = FALSE
					AND $7::boolean
			)
			SELECT DISTINCT m.buffer_id
			FROM doomed_target dt
			JOIN metadata m ON m.object_id = dt.id
			WHERE m.buffer_id IS NOT NULL
		`

		rows, err := tx.Query(ctx, collectQuery, share, oldPath, dir, name, newPath, acc.ID, force)
		if err != nil {
			return fmt.Errorf("failed to collect information for renaming file: %v", err)
		}
		var bids []uint64
		for rows.Next() {
			var bid uint64
			if err := rows.Scan(&bid); err != nil {
				rows.Close()
				return fmt.Errorf("failed to scan buffer ID: %v", err)
			}
			bids = append(bids, bid)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("failed to iterate through buffer IDs: %v", err)
		}
		rows.Close()

		if force {
			const deleteQuery = `
				WITH caller AS (
					SELECT id, workgroup
					FROM accounts
					WHERE id = $4
				),
				dst_parent AS (
					SELECT d.id, d.full_path
					FROM directories d
					JOIN accounts owner ON owner.id = d.account
					CROSS JOIN caller c
					WHERE d.share_name = $1
						AND d.full_path = $2
						AND (
							d.account = c.id
							OR (d.private = FALSE AND owner.workgroup = c.workgroup)
						)

					UNION ALL

					SELECT NULL::bigint, '/'
					FROM caller
					WHERE $2 = '/'
				),
				check_no_dir_conflict AS (
					SELECT 1
					FROM dst_parent p
					WHERE NOT EXISTS (
						SELECT 1
						FROM directories d
						WHERE d.share_name = $1
							AND d.full_path = $3
					)
				)
				DELETE FROM objects o
				USING check_no_dir_conflict c
				WHERE o.share_name = $1
					AND o.full_path = $3
					AND o.temporary = FALSE
			`

			if _, err := tx.Exec(ctx, deleteQuery, share, dir, newPath, acc.ID); err != nil {
				return fmt.Errorf("failed to delete existing destination file: %w", err)
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
				CROSS JOIN caller c
				WHERE o.share_name = $1
					AND o.full_path = $2
					AND o.temporary = FALSE
					AND o.account = c.id
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
						OR (d.private = FALSE AND owner.workgroup = c.workgroup)
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
				full_path = t.new_path,
				modified_at = NOW()
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

		return nil
	})
}

// RenameDirectory renames or moves a directory.
func (db *Database) RenameDirectory(acc Account, share string, oldPath, newPath string, force bool) error {
	oldPath = normalizePath(oldPath)
	newPath = normalizePath(newPath)
	dir, name := splitPath(newPath)
	if oldPath == "" || name == "" {
		return ErrNameInvalid
	}

	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
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
						OR (d.private = FALSE AND owner.workgroup = c.workgroup)
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
						OR (d.private = FALSE AND owner.workgroup = c.workgroup)
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
					AND $5 NOT LIKE s.full_path || '/%%'
			),
			delete_existing AS (
				DELETE FROM directories d
				USING target t
				WHERE $7::boolean
					AND d.share_name = $1
					AND d.full_path = t.new_path
			),
			update_subdirs AS (
				UPDATE directories d
				SET
					full_path = t.new_path || substring(d.full_path FROM length(t.old_path) + 1),
					modified_at = NOW()
				FROM target t, accounts owner, caller c
				WHERE owner.id = d.account
					AND d.share_name = $1
					AND d.id <> t.src_id
					AND d.full_path LIKE t.old_path || '/%%'
					AND (
						d.account = c.id
						OR (d.private = FALSE AND owner.workgroup = c.workgroup)
					)
			),
			update_files AS (
				UPDATE objects o
				SET
					full_path = t.new_path || substring(o.full_path FROM length(t.old_path) + 1),
					modified_at = NOW()
				FROM target t, accounts owner, caller c
				WHERE owner.id = o.account
					AND o.share_name = $1
					AND o.full_path LIKE t.old_path || '/%%'
					AND o.account = c.id
					AND o.temporary = FALSE
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

		tag, err := tx.Exec(ctx, query, share, oldPath, dir, name, newPath, acc.ID, force)
		if err != nil {
			return fmt.Errorf("failed to rename directory: %v", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteFile deletes a file.
func (db *Database) DeleteFile(acc Account, share string, path string) error {
	path = normalizePath(path)
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const collectQuery = `
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
					AND o.temporary = FALSE
					AND (
						o.account = c.id
						OR (
							od.private = FALSE
							AND owner.workgroup = c.workgroup
						)
					)
			)
			SELECT DISTINCT m.buffer_id
			FROM metadata m
			JOIN target t ON m.object_id = t.id
			WHERE m.buffer_id IS NOT NULL
		`

		rows, err := tx.Query(ctx, collectQuery, share, path, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to collect file buffers: %w", err)
		}

		var bids []uint64
		for rows.Next() {
			var bid uint64
			if err := rows.Scan(&bid); err != nil {
				rows.Close()
				return fmt.Errorf("failed to scan buffer ID: %w", err)
			}
			bids = append(bids, bid)
		}

		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("failed to iterate buffer IDs: %w", err)
		}

		rows.Close()

		const deleteQuery = `
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
					AND o.temporary = FALSE
					AND (
						o.account = c.id
						OR (
							od.private = FALSE
							AND owner.workgroup = c.workgroup
						)
					)
			),
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

		return nil
	})
}

// DeleteDirectory deletes a directory and all its contents.
func (db *Database) DeleteDirectory(acc Account, share string, path string) error {
	path = normalizePath(path)
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
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
						OR (d.private = FALSE AND owner.workgroup = c.workgroup)
					)
			),
			target_objects AS (
				SELECT o.id
				FROM objects o
				JOIN src s ON TRUE
				WHERE o.share_name = $1
					AND o.full_path LIKE s.full_path || '/%%'
					And o.temporary = FALSE
			)
			SELECT DISTINCT m.buffer_id
			FROM metadata m
			JOIN target_objects t ON m.object_id = t.id
			WHERE m.buffer_id IS NOT NULL
		`

		rows, err := tx.Query(ctx, collectQuery, share, path, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to collect directory buffers: %w", err)
		}

		var bids []uint64
		for rows.Next() {
			var bid uint64
			if err := rows.Scan(&bid); err != nil {
				rows.Close()
				return fmt.Errorf("failed to scan buffer ID: %w", err)
			}
			bids = append(bids, bid)
		}

		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("failed to iterate buffer IDs: %w", err)
		}

		rows.Close()

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
						OR (d.private = FALSE AND owner.workgroup = c.workgroup)
					)
			),
			delete_files AS (
				DELETE FROM objects o
				USING src s
				WHERE o.share_name = $1
					AND o.full_path LIKE s.full_path || '/%%'
					AND o.temporary = FALSE
				RETURNING o.id
			),
			delete_dirs AS (
				DELETE FROM directories d
				USING src s
				WHERE d.share_name = $1
					AND d.full_path LIKE s.full_path || '/%%'
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

		return nil
	})
}
