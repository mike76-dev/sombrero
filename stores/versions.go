package stores

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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
// A kept version is exempt from the count and age limits of the policy.
type Version struct {
	ID         uint64
	Size       uint64
	ModifiedAt time.Time
	ReplacedAt time.Time
	ReplacedBy string
	Kept       bool
}

// SetVersioning sets what a replaced file leaves behind.
func (db *Database) SetVersioning(p VersionPolicy) {
	db.versioning = p
}

// KeepsVersions says whether replaced files leave versions behind at all.
func (db *Database) KeepsVersions() bool {
	return db.versioning.Keep
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
	// A zero ID names the newest version: the contents that stood as the file
	// when the restore began, which its first write has made a version since.
	const query = `
		UPDATE objects o
		SET restored_from = v.id
		FROM objects v
		JOIN versions ON versions.object_id = v.id
		WHERE o.share_name = $1
			AND o.full_path = $2
			AND o.temporary = FALSE
			AND v.share_name = o.share_name
			AND v.full_path = o.full_path
			AND v.size = o.size
			AND (v.id = ANY($3::BIGINT[]) OR 0 = ANY($3::BIGINT[]) AND versions.replaced_at = (
				SELECT MAX(newest.replaced_at)
				FROM versions newest
				JOIN objects n ON n.id = newest.object_id
				WHERE n.share_name = o.share_name
					AND n.full_path = o.full_path
			))
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
// keeps, oldest first, and returns the slabs only they referenced. The kept
// versions neither count nor go.
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
			AND NOT v.kept
		ORDER BY v.replaced_at DESC, o.id DESC
		OFFSET $3
	`

	rows, err := tx.Query(ctx, extra, share, path, count)
	if err != nil {
		return nil, fmt.Errorf("failed to find the versions to drop: %w", err)
	}
	return dropVersions(ctx, tx, share, rows)
}

// KeepVersion exempts the version of the file at the path with the given ID
// from the count and age limits, or puts it back under them; none: ErrNotFound.
func (db *Database) KeepVersion(share, path string, id uint64, keep bool) error {
	path = normalizePath(path)
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			UPDATE versions v
			SET kept = $4
			FROM objects o
			WHERE o.id = v.object_id
				AND o.share_name = $1
				AND o.full_path = $2
				AND o.id = $3
		`

		tag, err := tx.Exec(ctx, query, share, path, int64(id), keep)
		if err != nil {
			return fmt.Errorf("failed to mark the version: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteVersions removes the version of the file at the path with the given
// ID, or every version of it with a zero ID, kept or not, and returns the
// slabs released.
func (db *Database) DeleteVersions(share, path string, id uint64) (slabs []types.Hash256, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		slabs, err = dropVersionsAt(ctx, tx, share, path, id)
		return err
	})
	return
}

// DeleteVersionsUnder drops the versions of every file under the folder, at
// any depth, kept or not, and returns the slabs released.
func (db *Database) DeleteVersionsUnder(share, dir string) (slabs []types.Hash256, err error) {
	prefix := strings.TrimSuffix(normalizePath(dir), "/") + "/"
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const under = `
			SELECT o.id, o.workgroup
			FROM versions v
			JOIN objects o ON o.id = v.object_id
			WHERE o.share_name = $1
				AND starts_with(o.full_path, $2)
		`

		rows, err := tx.Query(ctx, under, share, prefix)
		if err != nil {
			return fmt.Errorf("failed to find the versions: %w", err)
		}
		slabs, err = dropVersions(ctx, tx, share, rows)
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
		slabs, err = db.emptyFile(ctx, tx, share, path, acc, deliberate)
		return err
	})
	return
}

// emptyFile is EmptyFile within a transaction, which deleting a file on a share
// that keeps versions comes down to as well.
func (db *Database) emptyFile(ctx context.Context, tx pgx.Tx, share, path string, acc Account, deliberate bool) ([]types.Hash256, error) {
	const targets = deletableFile + `
		SELECT t.id
		FROM target t
		JOIN objects o ON o.id = t.id
		WHERE o.temporary = FALSE
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

	keep, err := db.keepsReplaced(ctx, tx, share, path, ids, uint64(acc.ID), deliberate)
	if err != nil {
		return nil, err
	}
	if keep {
		if err := recordVersions(ctx, tx, ids, uint64(acc.ID)); err != nil {
			return nil, err
		}
		return db.pruneVersions(ctx, tx, share, path)
	}

	const collect = `
		SELECT DISTINCT m.buffer_id, m.slab_key
		FROM metadata m
		WHERE m.object_id = ANY($1::BIGINT[])
	`

	rows, err = tx.Query(ctx, collect, ids)
	if err != nil {
		return nil, fmt.Errorf("failed to collect the storage of the file: %w", err)
	}
	bids, keys, err := collectStorage(rows)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM objects WHERE id = ANY($1::BIGINT[])`, ids); err != nil {
		return nil, fmt.Errorf("failed to delete the file: %w", err)
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

	var workgroup int
	if err := tx.QueryRow(ctx, `SELECT workgroup FROM accounts WHERE id = $1`, acc.ID).Scan(&workgroup); err != nil {
		return nil, fmt.Errorf("failed to resolve workgroup: %w", err)
	}
	return unreferencedSlabs(ctx, tx, share, workgroup, keys)
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
// the connections pick up. A zero age keeps the versions for good, as does the
// kept flag of a version.
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
				AND NOT v.kept
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
			SELECT o.id, o.size, o.modified_at, v.replaced_at, COALESCE(a.account_name, ''), v.kept
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
			if err := rows.Scan(&v.ID, &v.Size, &v.ModifiedAt, &v.ReplacedAt, &v.ReplacedBy, &v.Kept); err != nil {
				return fmt.Errorf("failed to scan version: %w", err)
			}
			versions = append(versions, v)
		}
		return rows.Err()
	})
	return
}

// filesAt picks, for each path the scope covers, the copy of the file that
// stood at the time $4 among the file as it is and its versions: one there by
// then and not replaced by then, with the contents of the time where they
// were kept, and the nearest kept after it otherwise. Times go by the second,
// as the snapshot names do. $1 is the share and $3 the account asking.
func filesAt(scope string) string {
	return `
		SELECT DISTINCT ON (o.full_path)
			o.full_path,
			o.id,
			o.size,
			o.created_at,
			o.modified_at,
			v.replaced_at
		FROM objects o
		LEFT JOIN versions v ON v.object_id = o.id
		JOIN accounts owner ON owner.id = o.account
		LEFT JOIN directories od
			ON od.share_name = o.share_name
			AND od.id = o.directory_id
		CROSS JOIN caller c
		WHERE o.share_name = $1
			AND ` + scope + `
			AND (o.temporary = FALSE OR v.object_id IS NOT NULL)
			AND date_trunc('second', o.created_at) <= $4
			AND (v.replaced_at IS NULL OR v.replaced_at > $4)
			AND (
				o.account = c.id
				OR (
					od.private = FALSE
					AND (owner.workgroup = c.workgroup OR owner.workgroup = anonymous_workgroup())
				)
				OR granted_level(o.share_name, c.workgroup, o.full_path) >= 0
				OR owns_ancestor(o.share_name, c.id, o.full_path)
			)
		ORDER BY o.full_path, date_trunc('second', o.modified_at) <= $4 DESC, v.replaced_at NULLS LAST, o.id
	`
}

// ListObjectsAt lists the directory as it was at the time: the folders made
// by then, and the files there by then with the contents they held, which are
// the versions replaced since, deleted files among them. A file whose state
// at the time was not kept is listed with the nearest state kept after it.
// A directory made after the time is ErrNotFound. The versions a deleted
// folder left have no directory, as the root's files have none: the root
// lists only the files whose paths put them there.
func (db *Database) ListObjectsAt(acc Account, shareName, path string, at time.Time) (objects []ObjectMeta, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		query := `
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
				AND date_trunc('second', d.created_at) <= $4
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

			SELECT full_path, size, created_at, modified_at, FALSE AS is_dir
			FROM (` + filesAt(`o.directory_id IS NOT DISTINCT FROM $2
				AND (o.directory_id IS NOT NULL OR position('/' IN substr(o.full_path, 2)) = 0)`) + `) f

			ORDER BY full_path
		`

		parentID, found, err := listedDirectory(ctx, tx, acc, shareName, path)
		if err != nil || !found {
			return err
		}
		if parentID != nil {
			var made time.Time
			if err := tx.QueryRow(ctx, `SELECT created_at FROM directories WHERE id = $1`, parentID).Scan(&made); err != nil {
				return fmt.Errorf("failed to query the directory: %w", err)
			}
			if made.Truncate(time.Second).After(at) {
				return ErrNotFound
			}
		}

		rows, err := tx.Query(ctx, query, shareName, parentID, acc.ID, at)
		if err != nil {
			return fmt.Errorf("failed to query the objects at the time: %w", err)
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

// ObjectAt finds the copy of the file at the path that stood at the time, as
// ListObjectsAt picks it: a version, or the file as it is now with a zero ID.
// The caller answers for the account's right to read the file.
func (db *Database) ObjectAt(acc Account, shareName, path string, at time.Time) (version Version, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		query := `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			)
			SELECT id, size, created_at, modified_at, replaced_at
			FROM (` + filesAt(`o.full_path = $2`) + `) f
		`
		var id int64
		var created time.Time
		var replaced *time.Time
		err := tx.QueryRow(ctx, query, shareName, path, acc.ID, at).Scan(&id, &version.Size, &created, &version.ModifiedAt, &replaced)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("failed to find the file at the time: %w", err)
		}
		if replaced != nil {
			version.ID, version.ReplacedAt = uint64(id), *replaced
		}
		return nil
	})
	return
}

// Snapshots lists the times the folder has previous states at, newest first:
// the times the versions of the files under it, at any depth, were last
// written, by the second. The caller answers for the right to the folder.
func (db *Database) Snapshots(shareName, path string) (times []time.Time, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT DISTINCT date_trunc('second', o.modified_at) AS at
			FROM objects o
			JOIN versions v ON v.object_id = o.id
			WHERE o.share_name = $1
				AND starts_with(o.full_path, $2)
			ORDER BY at DESC
		`
		prefix := strings.TrimSuffix(path, "/") + "/"
		rows, err := tx.Query(ctx, query, shareName, prefix)
		if err != nil {
			return fmt.Errorf("failed to list the snapshots: %w", err)
		}
		times, err = pgx.CollectRows(rows, pgx.RowTo[time.Time])
		return err
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
