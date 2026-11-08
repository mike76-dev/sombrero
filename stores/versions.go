package stores

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
)

// ErrUploadInFlight is returned where a file cannot be replaced because
// somebody else is writing it.
var ErrUploadInFlight = errors.New("the file is being written")

// VersionPolicy is what FinalizeUpload keeps of the file it replaces.
type VersionPolicy struct {
	// Keep says whether replaced files are kept as versions at all.
	Keep bool

	// Gap is the pause between two saves by the same account that separates
	// editing sessions, each leaving one version; zero keeps every save.
	Gap time.Duration

	// MaxSession is how long a session may run before a save leaves a version
	// anyway; zero puts no cap on it.
	MaxSession time.Duration

	// Count is how many versions of a file are kept, the oldest dropped first.
	Count int
}

// Version is a previous content of a file, held by a hidden object of its own.
type Version struct {
	ID         uint64
	Size       uint64
	ModifiedAt time.Time
	ReplacedAt time.Time
	ReplacedBy string
}

// SetVersioning sets what a replaced file leaves behind.
func (db *Database) SetVersioning(p VersionPolicy) {
	db.versioning = p
}

// keepsVersion says whether the file about to be replaced is kept: the policy
// must say so, and the save must not continue the editing session that left
// the newest version, which it does when it is the same account's, within the
// gap of the session's last save and within the cap of its start.
func (db *Database) keepsVersion(ctx context.Context, tx pgx.Tx, share, path string, account uint64) (bool, error) {
	p := db.versioning
	if !p.Keep {
		return false, nil
	}
	if p.Gap <= 0 {
		return true, nil
	}

	const query = `
		SELECT v.object_id,
			COALESCE(v.replaced_by = $3, FALSE)
			AND v.session_until > NOW() - make_interval(secs => $4)
			AND ($5 <= 0 OR v.replaced_at > NOW() - make_interval(secs => $5))
		FROM versions v
		JOIN objects o ON o.id = v.object_id
		WHERE o.share_name = $1
			AND o.full_path = $2
		ORDER BY v.replaced_at DESC, o.id DESC
		LIMIT 1
	`

	var vid uint64
	var continues bool
	err := tx.QueryRow(ctx, query, share, path, account, p.Gap.Seconds(), p.MaxSession.Seconds()).Scan(&vid, &continues)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("failed to look up the last version: %w", err)
	}
	if !continues {
		return true, nil
	}

	const extend = `
		UPDATE versions
		SET session_until = NOW()
		WHERE object_id = $1
	`

	if _, err := tx.Exec(ctx, extend, vid); err != nil {
		return false, fmt.Errorf("failed to extend the editing session: %w", err)
	}
	return false, nil
}

// keepsReplaced says whether the contents being replaced are kept: a deliberate replacement
// keeps them unless the file holds a restored version untouched; others follow the session rule.
func (db *Database) keepsReplaced(ctx context.Context, tx pgx.Tx, share, path string, ids []int64, account uint64, deliberate bool) (bool, error) {
	if !deliberate || !db.versioning.Keep {
		return db.keepsVersion(ctx, tx, share, path, account)
	}

	const query = `
		SELECT EXISTS (
			SELECT 1
			FROM objects o
			JOIN versions v ON v.object_id = o.restored_from
			WHERE o.id = ANY($1::BIGINT[])
		)
	`

	var restored bool
	if err := tx.QueryRow(ctx, query, ids).Scan(&restored); err != nil {
		return false, fmt.Errorf("failed to look up what the file was restored from: %w", err)
	}
	return !restored, nil
}

// markRestored notes that the file holds unchanged the one of the versions
// that is of its size, which a later restore need not keep; none: ErrNotFound.
func markRestored(ctx context.Context, tx pgx.Tx, share, path string, ids []uint64) error {
	const query = `
		UPDATE objects o
		SET restored_from = v.id
		FROM objects v
		JOIN versions ON versions.object_id = v.id
		WHERE o.share_name = $1
			AND o.full_path = $2
			AND o.temporary = FALSE
			AND v.id = ANY($3::BIGINT[])
			AND v.share_name = o.share_name
			AND v.full_path = o.full_path
			AND v.size = o.size
	`

	candidates := make([]int64, 0, len(ids))
	for _, id := range ids {
		candidates = append(candidates, int64(id))
	}
	tag, err := tx.Exec(ctx, query, share, path, candidates)
	if err != nil {
		return fmt.Errorf("failed to note the restored version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkRestored is markRestored for a file written over SMB from one of its
// versions. The caller answers for the account's right to write the file.
func (db *Database) MarkRestored(share, path string, ids []uint64) error {
	path = normalizePath(path)
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		return markRestored(ctx, tx, share, path, ids)
	})
}

// keepVersion moves the contents of the file onto a hidden row of their own
// and records it as a version replaced by the account.
func keepVersion(ctx context.Context, tx pgx.Tx, oid, account uint64) error {
	const copyRow = `
		INSERT INTO objects (share_name, directory_id, name, full_path, size, account, workgroup, created_at, modified_at, temporary)
		SELECT share_name, directory_id, name, full_path, size, account, workgroup, created_at, modified_at, TRUE
		FROM objects
		WHERE id = $1
		RETURNING id
	`

	var vid uint64
	if err := tx.QueryRow(ctx, copyRow, oid).Scan(&vid); err != nil {
		return fmt.Errorf("failed to keep the replaced file: %w", err)
	}

	const moveContents = `
		UPDATE metadata
		SET object_id = $1
		WHERE object_id = $2
	`

	if _, err := tx.Exec(ctx, moveContents, vid, oid); err != nil {
		return fmt.Errorf("failed to move the contents of the replaced file: %w", err)
	}

	const record = `
		INSERT INTO versions (object_id, replaced_by)
		VALUES ($1, $2)
	`

	if _, err := tx.Exec(ctx, record, vid, account); err != nil {
		return fmt.Errorf("failed to record the version: %w", err)
	}
	return nil
}

// pruneVersions drops the versions of the file beyond the count the policy
// keeps, oldest first, and returns the slabs only they referenced.
func (db *Database) pruneVersions(ctx context.Context, tx pgx.Tx, share, path string) ([]types.Hash256, error) {
	count := db.versioning.Count
	if count <= 0 {
		count = DefaultVersionCount
	}

	const extra = `
		SELECT o.id, o.workgroup
		FROM versions v
		JOIN objects o ON o.id = v.object_id
		WHERE o.share_name = $1
			AND o.full_path = $2
		ORDER BY v.replaced_at DESC, o.id DESC
		OFFSET $3
	`

	rows, err := tx.Query(ctx, extra, share, path, count)
	if err != nil {
		return nil, fmt.Errorf("failed to find the versions to drop: %w", err)
	}
	return dropVersions(ctx, tx, share, rows)
}

// DeleteVersions removes the version of the file at the path with the given
// ID, or every version of it with a zero ID, and returns the slabs released.
func (db *Database) DeleteVersions(share, path string, id uint64) (slabs []types.Hash256, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		slabs, err = dropVersionsAt(ctx, tx, share, path, id)
		return err
	})
	return
}

// EmptyFile removes the contents of the file ahead of their replacement, as a
// create that overwrites it does: they are kept as a version where the policy
// says, or whatever the session when the emptying is deliberate, the versions
// of the file stay, and the slabs released are returned.
func (db *Database) EmptyFile(acc Account, share, path string, deliberate bool) (slabs []types.Hash256, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const targets = deletableFile + `
			SELECT t.id
			FROM target t
			JOIN objects o ON o.id = t.id
			WHERE o.temporary = FALSE
		`

		rows, err := tx.Query(ctx, targets, share, path, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to find the file: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return fmt.Errorf("failed to read the file: %w", err)
		}
		if len(ids) == 0 {
			return ErrNotFound
		}

		keep, err := db.keepsReplaced(ctx, tx, share, path, ids, uint64(acc.ID), deliberate)
		if err != nil {
			return err
		}
		if keep {
			if err := recordVersions(ctx, tx, ids, uint64(acc.ID)); err != nil {
				return err
			}
			slabs, err = db.pruneVersions(ctx, tx, share, path)
			return err
		}

		const collect = `
			SELECT DISTINCT m.buffer_id, m.slab_key
			FROM metadata m
			WHERE m.object_id = ANY($1::BIGINT[])
		`

		rows, err = tx.Query(ctx, collect, ids)
		if err != nil {
			return fmt.Errorf("failed to collect the storage of the file: %w", err)
		}
		bids, keys, err := collectStorage(rows)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `DELETE FROM objects WHERE id = ANY($1::BIGINT[])`, ids); err != nil {
			return fmt.Errorf("failed to delete the file: %w", err)
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

		var workgroup int
		if err := tx.QueryRow(ctx, `SELECT workgroup FROM accounts WHERE id = $1`, acc.ID).Scan(&workgroup); err != nil {
			return fmt.Errorf("failed to resolve workgroup: %w", err)
		}
		slabs, err = unreferencedSlabs(ctx, tx, share, workgroup, keys)
		return err
	})
	return
}

// dropVersionsAt drops the version of the file at the path with the given ID,
// or every version of it with a zero ID, and returns the slabs released.
func dropVersionsAt(ctx context.Context, tx pgx.Tx, share, path string, id uint64) ([]types.Hash256, error) {
	const named = `
		SELECT o.id, o.workgroup
		FROM versions v
		JOIN objects o ON o.id = v.object_id
		WHERE o.share_name = $1
			AND o.full_path = $2
			AND ($3 = 0 OR o.id = $3)
	`

	rows, err := tx.Query(ctx, named, share, path, int64(id))
	if err != nil {
		return nil, fmt.Errorf("failed to find the versions: %w", err)
	}
	return dropVersions(ctx, tx, share, rows)
}

// recordVersions turns the visible files with the given IDs into versions
// replaced by the account, where they stand.
func recordVersions(ctx context.Context, tx pgx.Tx, ids []int64, account uint64) error {
	const hide = `
		UPDATE objects
		SET temporary = TRUE
		WHERE id = ANY($1::BIGINT[])
	`

	if _, err := tx.Exec(ctx, hide, ids); err != nil {
		return fmt.Errorf("failed to hide the replaced files: %w", err)
	}

	const record = `
		INSERT INTO versions (object_id, replaced_by)
		SELECT u.id, $2
		FROM UNNEST($1::BIGINT[]) AS u(id)
	`

	if _, err := tx.Exec(ctx, record, ids, account); err != nil {
		return fmt.Errorf("failed to record the versions: %w", err)
	}
	return nil
}

// versionRow names a version to drop: the object holding it, and the
// workgroup whose connection pinned its slabs.
type versionRow struct {
	id        int64
	workgroup int
}

// dropVersions deletes the versions the rows name, as IDs and workgroups, with
// their metadata and the buffers only they used. It returns the slabs only
// they referenced, staged for unpinning under the workgroup that pinned them.
func dropVersions(ctx context.Context, tx pgx.Tx, share string, rows pgx.Rows) ([]types.Hash256, error) {
	victims, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (v versionRow, err error) {
		err = row.Scan(&v.id, &v.workgroup)
		return
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read the versions to drop: %w", err)
	}
	return dropVersionRows(ctx, tx, share, victims)
}

// dropVersionRows is dropVersions for versions already read.
func dropVersionRows(ctx context.Context, tx pgx.Tx, share string, victims []versionRow) (slabs []types.Hash256, err error) {
	byWorkgroup := make(map[int][]int64)
	for _, v := range victims {
		byWorkgroup[v.workgroup] = append(byWorkgroup[v.workgroup], v.id)
	}

	for workgroup, ids := range byWorkgroup {
		const collect = `
			SELECT DISTINCT m.buffer_id, m.slab_key
			FROM metadata m
			WHERE m.object_id = ANY($1::BIGINT[])
		`

		rows, err := tx.Query(ctx, collect, ids)
		if err != nil {
			return nil, fmt.Errorf("failed to collect the storage of the versions: %w", err)
		}
		bids, keys, err := collectStorage(rows)
		if err != nil {
			return nil, err
		}

		const remove = `
			DELETE FROM objects
			WHERE id = ANY($1::BIGINT[])
		`

		if _, err := tx.Exec(ctx, remove, ids); err != nil {
			return nil, fmt.Errorf("failed to drop the versions: %w", err)
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
				return nil, fmt.Errorf("failed to delete orphaned buffer: %w", err)
			}
		}

		released, err := unreferencedSlabs(ctx, tx, share, workgroup, keys)
		if err != nil {
			return nil, err
		}
		slabs = append(slabs, released...)
	}
	return slabs, nil
}

// ExpireVersions drops the versions replaced longer ago than the age, on every
// share, and returns how many went. Their slabs are staged for unpinning, which
// the connections pick up. A zero age keeps the versions for good.
func (db *Database) ExpireVersions(age time.Duration) (dropped int, err error) {
	if age <= 0 {
		return 0, nil
	}

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const expired = `
			SELECT o.share_name, o.id, o.workgroup
			FROM versions v
			JOIN objects o ON o.id = v.object_id
			WHERE v.replaced_at < NOW() - make_interval(secs => $1)
		`

		rows, err := tx.Query(ctx, expired, age.Seconds())
		if err != nil {
			return fmt.Errorf("failed to find the expired versions: %w", err)
		}
		byShare := make(map[string][]versionRow)
		_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (share string, err error) {
			var v versionRow
			if err = row.Scan(&share, &v.id, &v.workgroup); err == nil {
				byShare[share] = append(byShare[share], v)
			}
			return
		})
		if err != nil {
			return fmt.Errorf("failed to read the expired versions: %w", err)
		}

		for share, victims := range byShare {
			if _, err := dropVersionRows(ctx, tx, share, victims); err != nil {
				return err
			}
			dropped += len(victims)
		}
		return nil
	})
	return
}

// RestoreVersion makes the version the contents of the file again. What it
// replaces is kept as a version, a restore being no autosave, unless it is a
// restored version itself, and the slabs released are returned, staged for
// unpinning. The version stays, sharing its slabs; its buffers are copied.
func (db *Database) RestoreVersion(acc Account, share, path string, id uint64) (slabs []types.Hash256, err error) {
	path = normalizePath(path)

	var exists bool
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT EXISTS (
				SELECT 1
				FROM versions v
				JOIN objects o ON o.id = v.object_id
				WHERE o.id = $1
					AND o.share_name = $2
					AND o.full_path = $3
			)
		`
		return tx.QueryRow(ctx, query, int64(id), share, path).Scan(&exists)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to look up the version: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}

	uploadID, err := db.CreateUpload(acc, share, path)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrUploadInFlight
	} else if err != nil {
		return nil, err
	}

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		return copyVersionContents(ctx, tx, uploadID, int64(id))
	})
	if err != nil {
		if _, rerr := db.RemoveUpload(uploadID); rerr != nil {
			return nil, fmt.Errorf("%w (and the upload could not be removed: %v)", err, rerr)
		}
		return nil, err
	}
	if slabs, err = db.finalizeUpload(uploadID, true); err != nil {
		return nil, err
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		return markRestored(ctx, tx, share, path, []uint64{id})
	})
	return slabs, err
}

// copyVersionContents gives the staged upload the pieces of the version: the
// slab pieces as they are, and the buffered ones as buffers of their own,
// queued for upload like a fresh write.
func copyVersionContents(ctx context.Context, tx pgx.Tx, uploadID string, versionID int64) error {
	uid, err := hex.DecodeString(uploadID)
	if err != nil {
		return err
	}

	const target = `
		SELECT u.id, u.object_id, o.share_name
		FROM uploads u
		JOIN objects o ON o.id = u.object_id
		WHERE u.upload_id = $1
	`

	var tuid, oid int64
	var share string
	if err := tx.QueryRow(ctx, target, uid).Scan(&tuid, &oid, &share); err != nil {
		return fmt.Errorf("failed to find the upload: %w", err)
	}

	const pieces = `
		SELECT obj_offset, slab_key, buffer_id, data_offset, data_length
		FROM metadata
		WHERE object_id = $1
		ORDER BY obj_offset
	`

	type piece struct {
		at, off, length int64
		key             []byte
		buffer          *int64
	}
	rows, err := tx.Query(ctx, pieces, versionID)
	if err != nil {
		return fmt.Errorf("failed to read the version's pieces: %w", err)
	}
	ps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (p piece, err error) {
		err = row.Scan(&p.at, &p.key, &p.buffer, &p.off, &p.length)
		return
	})
	if err != nil {
		return fmt.Errorf("failed to read the version's pieces: %w", err)
	}

	for _, p := range ps {
		if p.key != nil {
			const copySlab = `
				INSERT INTO metadata (object_id, obj_offset, slab_key, data_offset, data_length)
				VALUES ($1, $2, $3, $4, $5)
			`
			if _, err := tx.Exec(ctx, copySlab, oid, p.at, p.key, p.off, p.length); err != nil {
				return fmt.Errorf("failed to copy a slab piece: %w", err)
			}
			continue
		}

		const copyBuffer = `
			INSERT INTO buffers (share_name, data)
			SELECT $1, SUBSTRING(data FROM $2::INT + 1 FOR $3::INT)
			FROM buffers
			WHERE id = $4
			RETURNING id
		`
		var bid int64
		if err := tx.QueryRow(ctx, copyBuffer, share, p.off, p.length, *p.buffer).Scan(&bid); err != nil {
			return fmt.Errorf("failed to copy a buffer: %w", err)
		}

		const copyPiece = `
			INSERT INTO metadata (object_id, obj_offset, upload_id, buffer_id, data_offset, data_length)
			VALUES ($1, $2, $3, $4, 0, $5)
			RETURNING id
		`
		var mid int64
		if err := tx.QueryRow(ctx, copyPiece, oid, p.at, tuid, bid, p.length).Scan(&mid); err != nil {
			return fmt.Errorf("failed to copy a buffered piece: %w", err)
		}

		const queue = `
			INSERT INTO upload_jobs (upload_id, metadata_id)
			VALUES ($1, $2)
		`
		if _, err := tx.Exec(ctx, queue, tuid, mid); err != nil {
			return fmt.Errorf("failed to queue the buffered piece: %w", err)
		}
	}
	return nil
}

// Versions lists the versions of the file at the path, newest first. The
// caller answers for the account's right to read the file.
func (db *Database) Versions(share, path string) (versions []Version, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT o.id, o.size, o.modified_at, v.replaced_at, COALESCE(a.account_name, '')
			FROM versions v
			JOIN objects o ON o.id = v.object_id
			LEFT JOIN accounts a ON a.id = v.replaced_by
			WHERE o.share_name = $1
				AND o.full_path = $2
			ORDER BY v.replaced_at DESC, o.id DESC
		`

		rows, err := tx.Query(ctx, query, share, path)
		if err != nil {
			return fmt.Errorf("failed to list versions: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var v Version
			if err := rows.Scan(&v.ID, &v.Size, &v.ModifiedAt, &v.ReplacedAt, &v.ReplacedBy); err != nil {
				return fmt.Errorf("failed to scan version: %w", err)
			}
			versions = append(versions, v)
		}
		return rows.Err()
	})
	return
}

// VersionMetadata retrieves the metadata of the version of the file at the
// path that intersects the given range, clipped the way GetMetadata clips it.
func (db *Database) VersionMetadata(share, path string, id uint64, offset, length uint64) (slabs []SlabSlice, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			WITH target AS (
				SELECT o.id
				FROM objects o
				JOIN versions v ON v.object_id = o.id
				WHERE o.share_name = $1
					AND o.full_path = $2
					AND o.id = $3
			),
		` + clippedMetadata

		rows, err := tx.Query(ctx, query, share, path, int64(id), int64(offset), int64(length))
		if err != nil {
			return fmt.Errorf("failed to query version metadata: %v", err)
		}
		slabs, err = scanSlabSlices(rows)
		return err
	})
	return
}
