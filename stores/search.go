package stores

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// SearchLimit is the most matches a search returns.
const SearchLimit = 200

// SearchObjects finds the folders and files on the share whose names contain
// the text, whatever the case, among those the account may list, as ListObjects
// decides it, by path and up to SearchLimit of them.
func (db *Database) SearchObjects(acc Account, shareName, text string) (objects []ObjectMeta, err error) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil, nil
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $2
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
				AND position($3 IN lower(regexp_replace(d.full_path, '^.*/', ''))) > 0
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
				AND o.directory_id IS NOT DISTINCT FROM od.id
				AND o.temporary = FALSE
				AND position($3 IN lower(regexp_replace(o.full_path, '^.*/', ''))) > 0
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
			LIMIT $4
		`
		rows, err := tx.Query(ctx, query, shareName, acc.ID, text, SearchLimit)
		if err != nil {
			return fmt.Errorf("failed to search objects: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var obj ObjectMeta
			if err := rows.Scan(&obj.Path, &obj.Size, &obj.CreatedAt, &obj.ModifiedAt, &obj.IsDir); err != nil {
				return fmt.Errorf("failed to scan object: %w", err)
			}
			objects = append(objects, obj)
		}
		return rows.Err()
	})
	return
}
