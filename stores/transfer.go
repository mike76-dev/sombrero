package stores

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
)

// TransferTarget is where a description is applied: the share and the workgroup
// that take it on, and the account the folders and files come to belong to.
type TransferTarget struct {
	Share     string
	Workgroup int
	Owner     Account
}

// ApplyResult says what applying one file or folder came to.
type ApplyResult int

const (
	// Applied means the rows were written.
	Applied ApplyResult = iota

	// AlreadyThere means the path was taken and nothing was touched. Applying a
	// description twice is how an import that stopped halfway is resumed.
	AlreadyThere

	// Unresolved means a part of the file says only where its bytes could be
	// fetched or pinned from, which this server cannot act on: whoever applies
	// the description has to get at them first.
	Unresolved
)

// String implements fmt.Stringer.
func (r ApplyResult) String() string {
	switch r {
	case Applied:
		return "applied"
	case AlreadyThere:
		return "already there"
	case Unresolved:
		return "unresolved"
	default:
		return fmt.Sprintf("ApplyResult(%d)", int(r))
	}
}

// ApplyStats is what applying a whole description came to. Incomplete counts the
// files that were applied with runs of bytes no part described, which are holes
// in what was restored rather than a reason to leave the file out.
type ApplyStats struct {
	Directories  int
	Files        int
	AlreadyThere int
	Unresolved   int
	Incomplete   int
}

// ApplyTransfer reads a description and applies what it holds to the target,
// stopping where the context is cancelled or the stream is not whole.
func (db *Database) ApplyTransfer(ctx context.Context, r *transfer.Reader, target TransferTarget) (stats ApplyStats, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		dir, file, err := r.Next()
		if errors.Is(err, io.EOF) {
			return stats, nil
		}
		if err != nil {
			return stats, err
		}
		if err := db.applyRecord(target, dir, file, &stats); err != nil {
			return stats, err
		}
	}
}

// applyRecord applies one folder or file, whichever is set, and counts what came
// of it.
func (db *Database) applyRecord(target TransferTarget, dir *transfer.Directory, file *transfer.File, stats *ApplyStats) error {
	switch {
	case dir != nil:
		res, err := db.ApplyDirectory(target, *dir)
		if err != nil {
			return fmt.Errorf("failed to apply the folder %q: %w", dir.Path, err)
		}
		if res == Applied {
			stats.Directories++
		} else {
			stats.AlreadyThere++
		}

	case file != nil:
		res, err := db.ApplyFile(target, *file)
		if err != nil {
			return fmt.Errorf("failed to apply the file %q: %w", file.Path, err)
		}
		switch res {
		case Applied:
			stats.Files++
			if !file.Complete() {
				stats.Incomplete++
			}
		case AlreadyThere:
			stats.AlreadyThere++
		case Unresolved:
			stats.Unresolved++
		}
	}

	return nil
}

// ApplyDirectory creates the folder the description names, and the folders above
// it that are not there yet.
func (db *Database) ApplyDirectory(target TransferTarget, dir transfer.Directory) (ApplyResult, error) {
	if err := dir.Validate(); err != nil {
		return Applied, err
	}

	res := Applied
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		if err := indexdShare(ctx, tx, target.Share); err != nil {
			return err
		}

		_, created, err := ensureDirectory(ctx, tx, target, dir.Path, dir.Private, dir.ReadOnly, dir.CreatedAt, dir.ModifiedAt)
		if err != nil {
			return err
		}
		if !created {
			res = AlreadyThere
		}

		return nil
	})

	return res, err
}

// ApplyFile creates the file the description names, with the folders above it,
// and the metadata that says what its contents are made of.
func (db *Database) ApplyFile(target TransferTarget, file transfer.File) (ApplyResult, error) {
	if err := file.Validate(); err != nil {
		return Applied, err
	}

	// A part that carries neither the object its bytes are in nor the bytes
	// themselves is one this server cannot place, and a file is placed whole or
	// not at all.
	for _, part := range file.Parts {
		if part.Object == (types.Hash256{}) && part.Inline == nil {
			return Unresolved, nil
		}
	}

	dirPath, name := splitPath(normalizePath(file.Path))
	if name == "" {
		return Applied, ErrNameInvalid
	}

	res := Applied
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		if err := indexdShare(ctx, tx, target.Share); err != nil {
			return err
		}

		taken, err := pathTaken(ctx, tx, target.Share, normalizePath(file.Path))
		if err != nil {
			return err
		}
		if taken {
			res = AlreadyThere
			return nil
		}

		// A folder the description did not name is made for the workgroup rather
		// than for the one account, since nothing says it was ever private.
		dirID, _, err := ensureDirectory(ctx, tx, target, dirPath, false, false, file.CreatedAt, file.ModifiedAt)
		if err != nil {
			return err
		}

		objectID, err := insertObject(ctx, tx, target, dirID, name, normalizePath(file.Path), file)
		if err != nil {
			return err
		}

		for _, part := range file.Parts {
			if err := insertPart(ctx, tx, target.Share, objectID, part); err != nil {
				return err
			}
		}

		return nil
	})

	return res, err
}

// SetFileTimes puts back the times a file was made and last changed, which an
// upload of it stamps with the time of the upload instead.
func (db *Database) SetFileTimes(share, path string, createdAt, modifiedAt time.Time) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			UPDATE objects
			SET created_at = $3, modified_at = $4
			WHERE share_name = $1
			AND full_path = $2
			AND temporary = FALSE
		`
		tag, err := tx.Exec(ctx, query, share, normalizePath(path), at(createdAt), at(modifiedAt))
		if err != nil {
			return fmt.Errorf("failed to set the times of %q: %w", path, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: the file %q", ErrNotFound, path)
		}

		return nil
	})
}

// Recover makes files of the runs found inside a file nobody could name, and cuts
// those runs out of it, in one transaction: the file comes to hold what nothing
// recognized, back to back, and goes away once nothing is left. A found file that
// is there already is left as it is.
//
// The slab the runs are of stays referenced by the files made of what was cut
// out, so nothing is staged for unpinning: a file is cut to nothing only where
// files were found in all of it.
func (db *Database) Recover(target TransferTarget, from string, found []transfer.File, remainder []transfer.Part) error {
	if len(found) == 0 && len(remainder) == 0 {
		return fmt.Errorf("nothing was found in %q, so there is nothing to cut it down to", from)
	}
	for _, file := range found {
		if err := file.Validate(); err != nil {
			return err
		}
	}

	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		if err := indexdShare(ctx, tx, target.Share); err != nil {
			return err
		}

		for _, file := range found {
			path := normalizePath(file.Path)
			taken, err := pathTaken(ctx, tx, target.Share, path)
			if err != nil {
				return err
			}
			if taken {
				continue
			}

			dirPath, name := splitPath(path)
			dirID, _, err := ensureDirectory(ctx, tx, target, dirPath, false, false, file.CreatedAt, file.ModifiedAt)
			if err != nil {
				return err
			}
			objectID, err := insertObject(ctx, tx, target, dirID, name, path, file)
			if err != nil {
				return err
			}
			for _, part := range file.Parts {
				if err := insertPart(ctx, tx, target.Share, objectID, part); err != nil {
					return err
				}
			}
		}

		return cutFile(ctx, tx, target.Share, normalizePath(from), remainder)
	})
}

// cutFile replaces what the file is made of with the given runs, which it comes
// to hold one after the other, or deletes the file where there are none.
func cutFile(ctx context.Context, tx pgx.Tx, share, path string, remainder []transfer.Part) error {
	const lookup = `SELECT id FROM objects WHERE share_name = $1 AND full_path = $2 AND temporary = FALSE`
	var id uint64
	if err := tx.QueryRow(ctx, lookup, share, path).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the file %q", ErrNotFound, path)
		}
		return fmt.Errorf("failed to look up the file %q: %w", path, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM metadata WHERE object_id = $1`, id); err != nil {
		return fmt.Errorf("failed to cut %q: %w", path, err)
	}
	if len(remainder) == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM objects WHERE id = $1`, id); err != nil {
			return fmt.Errorf("failed to delete %q: %w", path, err)
		}

		return nil
	}

	var size uint64
	for _, part := range remainder {
		part.Offset = size
		if err := insertPart(ctx, tx, share, id, part); err != nil {
			return err
		}
		size += part.Length
	}
	if _, err := tx.Exec(ctx, `UPDATE objects SET size = $2 WHERE id = $1`, id, size); err != nil {
		return fmt.Errorf("failed to resize %q: %w", path, err)
	}

	return nil
}

// AdoptFolder makes the folder at the path, and the folders above it, the owner's
// own: made where they are not there, private, and handed over with everything
// in them where they are. It is for the folders the server keeps for itself in a
// share, which have to belong to whoever the server writes them as now.
func (db *Database) AdoptFolder(target TransferTarget, path string) error {
	path = normalizePath(path)
	if path == "/" {
		return ErrNameInvalid
	}

	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		if err := indexdShare(ctx, tx, target.Share); err != nil {
			return err
		}
		now := time.Now()
		if _, _, err := ensureDirectory(ctx, tx, target, path, true, false, now, now); err != nil {
			return err
		}

		// The path, the folders above it and everything under it, files alike:
		// a folder is of no use to its owner while one above it is somebody's.
		const folders = `
			UPDATE directories
			SET account = $3, workgroup = $4, private = TRUE
			WHERE share_name = $1
			AND (full_path = $2 OR full_path LIKE $2 || '/%' OR $2 LIKE full_path || '/%')
		`
		if _, err := tx.Exec(ctx, folders, target.Share, path, target.Owner.ID, target.Workgroup); err != nil {
			return fmt.Errorf("failed to adopt the folder %q: %w", path, err)
		}
		const files = `
			UPDATE objects
			SET account = $3, workgroup = $4
			WHERE share_name = $1 AND full_path LIKE $2 || '/%'
		`
		if _, err := tx.Exec(ctx, files, target.Share, path, target.Owner.ID, target.Workgroup); err != nil {
			return fmt.Errorf("failed to adopt the files in %q: %w", path, err)
		}

		return nil
	})
}

// ensureDirectory returns the id of the folder at the path, creating it and the
// folders above it where they are not there yet.
func ensureDirectory(ctx context.Context, tx pgx.Tx, target TransferTarget, path string, private, readOnly bool, createdAt, modifiedAt time.Time) (id *uint64, created bool, err error) {
	path = normalizePath(path)
	if path == "/" {
		return nil, false, nil
	}

	const lookup = `
		SELECT id
		FROM directories
		WHERE share_name = $1
		AND full_path = $2
	`
	var found uint64
	err = tx.QueryRow(ctx, lookup, target.Share, path).Scan(&found)
	if err == nil {
		return &found, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("failed to look up the folder %q: %w", path, err)
	}

	parentPath, name := splitPath(path)
	if name == "" {
		return nil, false, ErrNameInvalid
	}
	parentID, _, err := ensureDirectory(ctx, tx, target, parentPath, private, readOnly, createdAt, modifiedAt)
	if err != nil {
		return nil, false, err
	}

	const insert = `
		INSERT INTO directories (
			share_name,
			parent_id,
			name,
			full_path,
			account,
			workgroup,
			private,
			read_only,
			created_at,
			modified_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`
	var newID uint64
	if err := tx.QueryRow(
		ctx, insert,
		target.Share, parentID, name, path,
		target.Owner.ID, target.Workgroup,
		private, readOnly,
		at(createdAt), at(modifiedAt),
	).Scan(&newID); err != nil {
		return nil, false, fmt.Errorf("failed to create the folder %q: %w", path, err)
	}

	return &newID, true, nil
}

// indexdShare refuses a share whose files this store does not keep: a renterd
// share is listed by renterd, so rows applied to one would be read by nothing.
func indexdShare(ctx context.Context, tx pgx.Tx, share string) error {
	const query = `SELECT share_type FROM shares WHERE share_name = $1`
	var shareType string
	if err := tx.QueryRow(ctx, query, share).Scan(&shareType); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the share %q", ErrNotFound, share)
		}
		return fmt.Errorf("failed to look up the share %q: %w", share, err)
	}
	if shareType != "indexd" {
		return fmt.Errorf("a description is applied to an indexd share, and %q is %s", share, shareType)
	}

	return nil
}

// pathTaken reports whether a file or a folder of the share is at the path
// already, which is what makes applying a description twice harmless.
func pathTaken(ctx context.Context, tx pgx.Tx, share, path string) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1 FROM objects WHERE share_name = $1 AND full_path = $2
			UNION ALL
			SELECT 1 FROM directories WHERE share_name = $1 AND full_path = $2
		)
	`
	var taken bool
	if err := tx.QueryRow(ctx, query, share, path).Scan(&taken); err != nil {
		return false, fmt.Errorf("failed to look up the path %q: %w", path, err)
	}

	return taken, nil
}

// insertObject writes the file itself, which is no longer of an upload: what it
// is made of comes from the description rather than from a client.
func insertObject(ctx context.Context, tx pgx.Tx, target TransferTarget, dirID *uint64, name, path string, file transfer.File) (uint64, error) {
	const query = `
		INSERT INTO objects (
			share_name,
			directory_id,
			name,
			full_path,
			size,
			account,
			workgroup,
			created_at,
			modified_at,
			temporary
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, FALSE)
		RETURNING id
	`
	var id uint64
	if err := tx.QueryRow(
		ctx, query,
		target.Share, dirID, name, path, file.Size,
		target.Owner.ID, target.Workgroup,
		at(file.CreatedAt), at(file.ModifiedAt),
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("failed to create the file %q: %w", path, err)
	}

	return id, nil
}

// insertPart writes what one run of a file's bytes is made of: the object that
// holds them, or the bytes themselves, which go on the queue to be uploaded.
func insertPart(ctx context.Context, tx pgx.Tx, share string, objectID uint64, part transfer.Part) error {
	if part.Object != (types.Hash256{}) {
		const query = `
			INSERT INTO metadata (object_id, obj_offset, slab_key, data_offset, data_length)
			VALUES ($1, $2, $3, $4, $5)
		`
		if _, err := tx.Exec(ctx, query, objectID, part.Offset, part.Object[:], part.DataOffset, part.Length); err != nil {
			return fmt.Errorf("failed to add the metadata at %d: %w", part.Offset, err)
		}

		return nil
	}

	// Carried bytes enter as buffered data with a job of their own, which is
	// what the packer uploads them with, the same as data written by a client.
	const query = `
		WITH new_buffer AS (
			INSERT INTO buffers (share_name, data)
			VALUES ($1, $2)
			RETURNING id
		),
		new_metadata AS (
			INSERT INTO metadata (object_id, obj_offset, buffer_id, data_offset, data_length)
			SELECT $3, $4, nb.id, 0, octet_length($2)
			FROM new_buffer nb
			RETURNING id
		)
		INSERT INTO upload_jobs (metadata_id)
		SELECT nm.id FROM new_metadata nm
	`
	if _, err := tx.Exec(ctx, query, share, part.Inline, objectID, part.Offset); err != nil {
		return fmt.Errorf("failed to buffer the bytes at %d: %w", part.Offset, err)
	}

	return nil
}

// at is the time to store, which is now where the description did not say.
func at(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}

	return t
}
