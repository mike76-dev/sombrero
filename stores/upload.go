package stores

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.sia.tech/core/types"
	"lukechampine.com/frand"
)

// SlabSlice represents a slice of data within an uploaded slab.
type SlabSlice struct {
	Key    types.Hash256
	Offset uint64
	Length uint64
	At     uint64
	Data   []byte
}

// uploadJob represents a pending upload job that is being processed asynchronously.
type UploadJob struct {
	ID         uint64
	UploadID   uint64
	MetadataID uint64
	ObjectID   uint64
	BufferID   uint64
	ObjOffset  uint64
	DataOffset uint64
	DataLength uint64
	Data       []byte
}

// ErrNoUploadJobs is returned when there are no pending upload jobs available for processing.
var ErrNoUploadJobs = errors.New("no upload jobs available")

// collectStorage scans pairs of buffer IDs and slab keys, as returned by the
// queries that gather the storage referenced by a set of metadata entries.
// Exactly one of the two is set in any given row.
func collectStorage(rows pgx.Rows) (bids []uint64, keys [][]byte, err error) {
	defer rows.Close()

	for rows.Next() {
		var (
			bid *uint64
			key []byte
		)
		if err := rows.Scan(&bid, &key); err != nil {
			return nil, nil, fmt.Errorf("failed to scan storage reference: %w", err)
		}
		if bid != nil {
			bids = append(bids, *bid)
		}
		if key != nil {
			keys = append(keys, key)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("failed to iterate storage references: %w", err)
	}

	return bids, keys, nil
}

// unreferencedSlabs returns those of the given slab keys that no metadata entry
// references any more. It has to be called after the referencing metadata has
// been deleted, so that slabs which are still shared with surviving files stay
// pinned.
func unreferencedSlabs(ctx context.Context, tx pgx.Tx, keys [][]byte) (slabs []types.Hash256, err error) {
	if len(keys) == 0 {
		return nil, nil
	}

	const query = `
		SELECT k
		FROM UNNEST($1::BYTEA[]) AS k
		WHERE NOT EXISTS (
			SELECT 1
			FROM metadata m
			WHERE m.slab_key = k
		)
	`

	rows, err := tx.Query(ctx, query, keys)
	if err != nil {
		return nil, fmt.Errorf("failed to filter unreferenced slabs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("failed to scan slab key: %w", err)
		}
		if len(raw) != 32 {
			return nil, fmt.Errorf("invalid slab key length: %d", len(raw))
		}
		var h types.Hash256
		copy(h[:], raw)
		slabs = append(slabs, h)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate slab keys: %w", err)
	}

	return slabs, nil
}

// CreateUpload creates a new upload entry in the database and returns the generated upload ID.
func (db *Database) CreateUpload(acc Account, share, path string) (uploadID string, err error) {
	path = normalizePath(path)
	dir, name := splitPath(path)
	if name == "" {
		return "", ErrNameInvalid
	}

	id := make([]byte, 32)
	frand.Read(id)
	uploadID = hex.EncodeToString(id)

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			),
			parent AS (
				SELECT d.id
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

				SELECT NULL::bigint
				FROM caller
				WHERE $2 = '/'
			),
			-- Only an upload whose object is still temporary is in flight. Once it
			-- has been finalized, its uploads entry may linger until the buffered
			-- slabs have made it to the Sia network, and must not block the path.
			no_existing_upload AS (
				SELECT 1
				FROM parent p
				WHERE NOT EXISTS (
					SELECT 1
					FROM uploads u
					JOIN objects o ON o.id = u.object_id
					WHERE o.share_name = $1
						AND o.full_path = $4
						AND o.temporary = TRUE
				)
			),
			not_read_only AS (
				SELECT 1
				FROM parent p
				CROSS JOIN caller c
				WHERE NOT EXISTS (
					SELECT 1
					FROM objects o
					JOIN directories d
						ON d.share_name = o.share_name
						AND d.id = o.directory_id
					WHERE o.share_name = $1
						AND o.full_path = $4
						AND o.temporary = FALSE
						AND o.account <> c.id
						AND d.read_only = TRUE
				)
			),
			staging AS (
				INSERT INTO objects (
					share_name,
					directory_id,
					name,
					full_path,
					size,
					account,
					workgroup,
					temporary
				)
				SELECT
					$1,
					p.id,
					$5,
					$4,
					0,
					c.id,
					c.workgroup,
					TRUE
				FROM parent p
				JOIN no_existing_upload n ON TRUE
				JOIN not_read_only r ON TRUE
				CROSS JOIN caller c
				RETURNING id
			)
			INSERT INTO uploads (upload_id, object_id)
			SELECT $6, s.id
			FROM staging s
			RETURNING id
		`

		var uid uint64
		if err = tx.QueryRow(ctx, query, share, dir, acc.ID, path, name, id).Scan(&uid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("failed to create upload: %w", err)
		}
		return nil
	})

	if err != nil {
		return "", err
	}
	return
}

// RemoveUpload deletes an upload entry from the database and removes any associated
// buffers that are not referenced by other uploads or metadata entries.
func (db *Database) RemoveUpload(uploadID string) (slabs []types.Hash256, err error) {
	id, err := hex.DecodeString(uploadID)
	if err != nil {
		return nil, err
	}

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const lookup = `
			SELECT u.id, u.object_id
			FROM uploads u
			JOIN objects o ON o.id = u.object_id
			WHERE u.upload_id = $1
				AND o.temporary = TRUE
		`

		var uid, soid uint64
		if err := tx.QueryRow(ctx, lookup, id).Scan(&uid, &soid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("failed to lookup upload: %w", err)
		}

		const collectStorageQuery = `
			SELECT DISTINCT m.buffer_id, m.slab_key
			FROM metadata m
			WHERE m.object_id = $1
		`

		rows, err := tx.Query(ctx, collectStorageQuery, soid)
		if err != nil {
			return fmt.Errorf("failed to collect storage: %w", err)
		}

		bids, keys, err := collectStorage(rows)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `DELETE FROM uploads WHERE id = $1`, uid); err != nil {
			return fmt.Errorf("failed to delete upload: %w", err)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM objects WHERE id = $1`, soid); err != nil {
			return fmt.Errorf("failed to delete temporary object: %w", err)
		}

		for _, id := range bids {
			if _, err := tx.Exec(ctx, `
				DELETE FROM buffers b
				WHERE b.id = $1
					AND NOT EXISTS (
						SELECT 1
						FROM metadata m
						WHERE m.buffer_id = b.id
					)
			`, id); err != nil {
				return fmt.Errorf("failed to delete orphaned buffer: %w", err)
			}
		}

		// The metadata is gone by now, so any slab that is still referenced
		// belongs to another file and must stay pinned.
		slabs, err = unreferencedSlabs(ctx, tx, keys)
		return err
	})

	if err != nil {
		return nil, err
	}
	return
}

// AddBufferedSlab adds a new buffered slab entry and associates it with
// the given upload ID and offset.
func (db *Database) AddBufferedSlab(uploadID string, offset uint64, data []byte) error {
	id, err := hex.DecodeString(uploadID)
	if err != nil {
		return err
	}

	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			WITH target_upload AS (
				SELECT u.id, u.object_id, o.share_name
				FROM uploads u
				JOIN objects o ON o.id = u.object_id
				WHERE u.upload_id = $1
			),
			new_buffer AS (
				INSERT INTO buffers (share_name, data)
				SELECT tu.share_name, $2
				FROM target_upload tu
				RETURNING id
			),
			new_metadata AS (
				INSERT INTO metadata (
					object_id,
					obj_offset,
					upload_id,
					buffer_id,
					data_offset,
					data_length
				)
				SELECT
					tu.object_id,
					$3,
					tu.id,
					nb.id,
					0,
					octet_length($2)
				FROM target_upload tu
				CROSS JOIN new_buffer nb
				RETURNING id
			)
			INSERT INTO upload_jobs (upload_id, metadata_id)
			SELECT tu.id, nm.id
			FROM target_upload tu
			CROSS JOIN new_metadata nm
		`

		tag, err := tx.Exec(ctx, query, id, data, offset)
		if err != nil {
			return fmt.Errorf("failed to add buffered slab: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ClaimUploadJob retrieves and locks the next pending upload job for processing.
func (db *Database) ClaimUploadJob(minSize uint64) (job UploadJob, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			WITH picked AS (
				SELECT
					uj.id,
					uj.upload_id,
					uj.metadata_id
				FROM upload_jobs uj
				JOIN metadata m ON m.id = uj.metadata_id
				JOIN buffers b ON b.id = m.buffer_id
				WHERE m.data_length >= $1
				ORDER BY uj.created_at
				FOR UPDATE SKIP LOCKED
				LIMIT 1
			),
			deleted AS (
				DELETE FROM upload_jobs uj
				USING picked p
				WHERE uj.id = p.id
				RETURNING uj.id, uj.upload_id, uj.metadata_id
			)
			SELECT
				d.id,
				d.upload_id,
				m.id,
				m.object_id,
				m.buffer_id,
				m.obj_offset,
				m.data_offset,
				m.data_length,
				b.data
			FROM deleted d
			JOIN metadata m ON m.id = d.metadata_id
			JOIN buffers b ON b.id = m.buffer_id
		`

		var data []byte
		err := tx.QueryRow(ctx, query, minSize).Scan(
			&job.ID,
			&job.UploadID,
			&job.MetadataID,
			&job.ObjectID,
			&job.BufferID,
			&job.ObjOffset,
			&job.DataOffset,
			&job.DataLength,
			&data,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNoUploadJobs
			}
			return fmt.Errorf("failed to claim upload job: %w", err)
		}

		end := job.DataOffset + job.DataLength
		if end > uint64(len(data)) {
			return fmt.Errorf("buffer slice out of bounds: offset %d, length %d, buffer size %d", job.DataOffset, job.DataLength, len(data))
		}
		job.Data = data[job.DataOffset:end]
		return nil
	})
	return
}

// maxPackedItems is the largest number of buffers that may be combined into a
// single packed slab. It bounds the work done by one claim; a share with more
// pending buffers than this simply fills its slabs over several passes.
const maxPackedItems = 256

// ClaimPackedSlab claims the buffers that together fill one slab of the given
// share, so that they can be uploaded as a single packed slab. The buffers are
// returned in the order in which they are to be concatenated, and the last one
// commonly overshoots the slab boundary; it is up to the caller to only use as
// much of it as fits.
//
// Only buffers belonging to finalized uploads are eligible. A buffer whose
// upload is still in flight may yet be abandoned or replaced, which would leave
// the data of an aborted upload sitting in a slab that has already been paid
// for.
//
// A positive maxAge also claims buffers that fall short of a slab, once the
// oldest of them has been waiting that long and they amount to at least minSize
// bytes. Such a claim fills the slab only partly, which costs as much as a full
// one, so with maxAge left at zero the buffers wait for as long as it takes.
//
// ErrNoUploadJobs is returned when neither applies, in which case nothing is
// claimed.
func (db *Database) ClaimPackedSlab(share string, slabSize, minSize uint64, maxAge time.Duration) (jobs []UploadJob, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		// The candidates are locked first, because a running total cannot be
		// computed over a locking select. Only the queue entries are locked,
		// so that the claim does not collide with unrelated work on the
		// objects and their metadata.
		//
		// A buffer that already fills a slab on its own is left to
		// ClaimUploadJob, which makes the two claims disjoint.
		const query = `
			WITH locked AS (
				SELECT
					uj.id,
					uj.upload_id,
					uj.metadata_id,
					uj.created_at,
					m.object_id,
					m.buffer_id,
					m.obj_offset,
					m.data_offset,
					m.data_length
				FROM upload_jobs uj
				JOIN metadata m ON m.id = uj.metadata_id
				JOIN objects o ON o.id = m.object_id
				WHERE o.share_name = $1
					AND m.buffer_id IS NOT NULL
					AND m.upload_id IS NULL
					AND m.data_length < $2::BIGINT
				ORDER BY m.object_id, m.obj_offset
				LIMIT $3
				FOR UPDATE OF uj SKIP LOCKED
			),
			running AS (
				SELECT
					l.*,
					SUM(l.data_length) OVER (
						ORDER BY l.object_id, l.obj_offset, l.id
					) AS total
				FROM locked l
			),
			available AS (
				SELECT
					COALESCE(SUM(data_length), 0) AS total,
					MIN(created_at) AS oldest
				FROM locked
			),
			picked AS (
				SELECT r.*
				FROM running r
				CROSS JOIN available a
				WHERE r.total - r.data_length < $2::BIGINT
					AND (
						a.total >= $2::BIGINT
						OR (
							$4::DOUBLE PRECISION > 0
							AND a.total >= $5::BIGINT
							AND a.oldest <= NOW() - MAKE_INTERVAL(secs => $4::DOUBLE PRECISION)
						)
					)
			),
			-- Data-modifying CTEs always run to completion, so the claimed
			-- entries leave the queue whether or not this is read below.
			deleted AS (
				DELETE FROM upload_jobs uj
				USING picked p
				WHERE uj.id = p.id
				RETURNING uj.id
			)
			SELECT
				p.id,
				p.upload_id,
				p.metadata_id,
				p.object_id,
				p.buffer_id,
				p.obj_offset,
				p.data_offset,
				p.data_length,
				SUBSTRING(b.data FROM p.data_offset::INT + 1 FOR p.data_length::INT)
			FROM picked p
			JOIN buffers b ON b.id = p.buffer_id
			ORDER BY p.object_id, p.obj_offset, p.id
		`

		rows, err := tx.Query(ctx, query, share, int64(slabSize), maxPackedItems, maxAge.Seconds(), int64(minSize))
		if err != nil {
			return fmt.Errorf("failed to claim packed slab: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var job UploadJob
			if err := rows.Scan(
				&job.ID,
				&job.UploadID,
				&job.MetadataID,
				&job.ObjectID,
				&job.BufferID,
				&job.ObjOffset,
				&job.DataOffset,
				&job.DataLength,
				&job.Data,
			); err != nil {
				return fmt.Errorf("failed to scan packed slab item: %w", err)
			}
			if uint64(len(job.Data)) != job.DataLength {
				return fmt.Errorf("buffer slice out of bounds: offset %d, length %d, slice size %d", job.DataOffset, job.DataLength, len(job.Data))
			}
			jobs = append(jobs, job)
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("failed to iterate packed slab items: %w", err)
		}

		if len(jobs) == 0 {
			return ErrNoUploadJobs
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return
}

// CleanupUploadJobs removes upload jobs whose metadata no longer references a buffer,
// e.g. after a requeued job was completed by another worker.
func (db *Database) CleanupUploadJobs() error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			DELETE FROM upload_jobs uj
			WHERE NOT EXISTS (
				SELECT 1
				FROM metadata m
				JOIN buffers b ON b.id = m.buffer_id
				WHERE m.id = uj.metadata_id
			)
		`

		if _, err := tx.Exec(ctx, query); err != nil {
			return fmt.Errorf("failed to clean up stale upload jobs: %w", err)
		}
		return nil
	})
}

// CompleteUploadJob marks the given upload job as completed by associating the provided slab key
// with the corresponding metadata entry and removing the buffer if it is no longer referenced.
func (db *Database) CompleteUploadJob(metadataID uint64, bufferID uint64, slabKey types.Hash256) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const updateQuery = `
			UPDATE metadata
			SET
				slab_key = $3,
				buffer_id = NULL,
				upload_id = NULL
			WHERE id = $1
				AND buffer_id = $2
		`

		tag, err := tx.Exec(ctx, updateQuery, metadataID, bufferID, slabKey[:])
		if err != nil {
			return fmt.Errorf("failed to update metadata: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var (
				currentBid  *uint64
				currentSlab []byte
			)

			const checkQuery = `
				SELECT buffer_id, slab_key
				FROM metadata
				WHERE id = $1
			`

			err := tx.QueryRow(ctx, checkQuery, metadataID).Scan(&currentBid, &currentSlab)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return fmt.Errorf("failed to verify metadata state: %w", err)
			}

			if currentBid == nil && len(currentSlab) == len(slabKey) {
				var existing types.Hash256
				copy(existing[:], currentSlab)
				if existing == slabKey {
					return nil
				}
				return fmt.Errorf("metadata %d already completed with different slab key", metadataID)
			}

			return ErrNotFound
		}

		const deleteQuery = `
			DELETE FROM buffers b
			WHERE b.id = $1
				AND NOT EXISTS (
					SELECT 1
					FROM metadata m
					WHERE m.buffer_id = b.id
				)
		`

		if _, err := tx.Exec(ctx, deleteQuery, bufferID); err != nil {
			return fmt.Errorf("failed to delete orphaned buffer: %w", err)
		}

		return nil
	})
}

// packedItemDone reports whether the metadata entry has already been completed
// with the given slab key, which is the case when a batch is retried after it
// had partly gone through. An entry that has disappeared is reported as not
// done, so that a slab none of whose items survived can be unpinned.
func packedItemDone(ctx context.Context, tx pgx.Tx, metadataID uint64, slabKey types.Hash256) (bool, error) {
	const query = `
		SELECT buffer_id, slab_key
		FROM metadata
		WHERE id = $1
	`

	var (
		bid *uint64
		raw []byte
	)

	if err := tx.QueryRow(ctx, query, metadataID).Scan(&bid, &raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("failed to verify metadata state: %w", err)
	}

	if bid != nil {
		return false, fmt.Errorf("metadata %d references an unexpected buffer", metadataID)
	}
	if len(raw) != len(slabKey) {
		return false, fmt.Errorf("invalid slab key length: %d", len(raw))
	}

	var existing types.Hash256
	copy(existing[:], raw)
	if existing != slabKey {
		return false, fmt.Errorf("metadata %d already completed with different slab key", metadataID)
	}

	return true, nil
}

// splitPackedItem moves the part of the item's buffer that did not fit into the
// slab into a buffer of its own and queues it, so that it becomes the head of
// the next packed slab. The remainder is copied inside the database, which
// keeps the bytes that did make it into the slab from lingering there.
func splitPackedItem(ctx context.Context, tx pgx.Tx, job UploadJob, taken uint64) error {
	// The entry stays free of an upload ID: only buffers of finalized
	// uploads are packed, and the remainder of one is no different.
	const query = `
		WITH remainder AS (
			INSERT INTO buffers (share_name, data)
			SELECT
				b.share_name,
				SUBSTRING(b.data FROM $2::INT + 1 FOR $3::INT)
			FROM buffers b
			WHERE b.id = $1
			RETURNING id
		),
		entry AS (
			INSERT INTO metadata (
				object_id,
				obj_offset,
				buffer_id,
				data_offset,
				data_length
			)
			SELECT $4, $5, r.id, 0, $3
			FROM remainder r
			RETURNING id
		)
		INSERT INTO upload_jobs (upload_id, metadata_id)
		SELECT $6, e.id
		FROM entry e
	`

	rest := job.DataLength - taken
	tag, err := tx.Exec(
		ctx,
		query,
		job.BufferID,
		int64(job.DataOffset+taken),
		int64(rest),
		job.ObjectID,
		int64(job.ObjOffset+taken),
		job.UploadID,
	)
	if err != nil {
		return fmt.Errorf("failed to split buffer %d: %w", job.BufferID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("failed to split buffer %d: buffer is gone", job.BufferID)
	}

	return nil
}

// CompletePackedSlab marks a batch of buffers as uploaded under the given slab
// key. The items are laid out in the slab in the order in which they are
// passed, each one at the offset that follows the previous one.
//
// The data of the last item may have been trimmed by the caller to what fitted
// into the slab, in which case the remainder is moved into a buffer of its own
// and queued for the next packed slab.
//
// Items whose metadata has meanwhile disappeared, e.g. because the file was
// deleted while the slab was being uploaded, are skipped. ErrNotFound is only
// returned when nothing of the batch is left, which is the one case where the
// caller may unpin the slab it has just uploaded.
func (db *Database) CompletePackedSlab(jobs []UploadJob, slabKey types.Hash256) error {
	if len(jobs) == 0 {
		return errors.New("cannot complete an empty packed slab")
	}

	for i, job := range jobs {
		taken := uint64(len(job.Data))
		if taken == 0 || taken > job.DataLength {
			return fmt.Errorf("item %d takes %d of %d buffered bytes", i, taken, job.DataLength)
		}
		if taken < job.DataLength && i != len(jobs)-1 {
			return fmt.Errorf("item %d is split, but only the last item of a packed slab may be", i)
		}
	}

	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		var completed, offset uint64
		for _, job := range jobs {
			taken := uint64(len(job.Data))

			// The length only changes for a split item, for which it
			// shrinks to the part that made it into the slab.
			const updateQuery = `
				UPDATE metadata
				SET
					slab_key = $3,
					data_offset = $4,
					data_length = $5,
					buffer_id = NULL,
					upload_id = NULL
				WHERE id = $1
					AND buffer_id = $2
			`

			tag, err := tx.Exec(ctx, updateQuery, job.MetadataID, job.BufferID, slabKey[:], int64(offset), int64(taken))
			if err != nil {
				return fmt.Errorf("failed to update metadata: %w", err)
			}

			offset += taken

			if tag.RowsAffected() == 0 {
				// Either the entry is gone or this batch has already been
				// completed, in which case its remainder was queued then.
				done, err := packedItemDone(ctx, tx, job.MetadataID, slabKey)
				if err != nil {
					return err
				}
				if done {
					completed++
				}
				continue
			}

			completed++

			if taken < job.DataLength {
				if err := splitPackedItem(ctx, tx, job, taken); err != nil {
					return err
				}
			}

			const deleteQuery = `
				DELETE FROM buffers b
				WHERE b.id = $1
					AND NOT EXISTS (
						SELECT 1
						FROM metadata m
						WHERE m.buffer_id = b.id
					)
			`

			if _, err := tx.Exec(ctx, deleteQuery, job.BufferID); err != nil {
				return fmt.Errorf("failed to delete orphaned buffer: %w", err)
			}
		}

		if completed == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RequeueUploadJob re-adds the given upload job to the queue for retrying.
func (db *Database) RequeueUploadJob(uploadID, metadataID uint64) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			INSERT INTO upload_jobs (upload_id, metadata_id)
			VALUES ($1, $2)
			ON CONFLICT (metadata_id) DO NOTHING
		`

		_, err := tx.Exec(ctx, query, uploadID, metadataID)
		if err != nil {
			return fmt.Errorf("failed to requeue upload job: %w", err)
		}
		return nil
	})
}

// FinalizeUpload finalizes the upload by making the associated object visible.
func (db *Database) FinalizeUpload(uploadID string) error {
	id, err := hex.DecodeString(uploadID)
	if err != nil {
		return err
	}

	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		var uid, soid uint64
		var share, path string
		var aid uint64
		const lookup = `
			SELECT u.id, u.object_id, o.share_name, o.full_path, o.account
			FROM uploads u
			JOIN objects o ON o.id = u.object_id
			WHERE u.upload_id = $1
				AND o.temporary = TRUE
		`

		if err := tx.QueryRow(ctx, lookup, id).Scan(&uid, &soid, &share, &path, &aid); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("failed to lookup upload: %w", err)
		}

		var ok bool
		const validate = `
			SELECT NOT EXISTS (
				SELECT 1
				FROM metadata
				WHERE object_id = $1
					AND (
						(slab_key IS NULL AND buffer_id IS NULL)
						OR
						(slab_key IS NOT NULL AND buffer_id IS NOT NULL)
					)
			)
		`

		if err := tx.QueryRow(ctx, validate, soid).Scan(&ok); err != nil {
			return fmt.Errorf("failed to validate metadata state: %w", err)
		}
		if !ok {
			return errors.New("cannot finalize upload: invalid metadata state")
		}

		var oid uint64
		const findVisible = `
			SELECT id
			FROM objects
			WHERE share_name = $1
				AND full_path = $2
				AND temporary = FALSE
		`

		err = tx.QueryRow(ctx, findVisible, share, path).Scan(&oid)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("failed to find visible object: %w", err)
		}

		finalOid := soid

		if errors.Is(err, pgx.ErrNoRows) {
			const makeVisible = `
				UPDATE objects o
				SET
					temporary = FALSE,
					modified_at = NOW(),
					size = COALESCE((
						SELECT SUM(data_length)
						FROM metadata
						WHERE object_id = o.id
					), 0)
				WHERE o.id = $1
			`

			tag, err := tx.Exec(ctx, makeVisible, soid)
			if err != nil {
				return fmt.Errorf("failed to make object visible: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		} else {
			const collectBuffers = `
				SELECT DISTINCT buffer_id
				FROM metadata m
				WHERE m.object_id = $1
					AND m.buffer_id IS NOT NULL
			`

			rows, err := tx.Query(ctx, collectBuffers, oid)
			if err != nil {
				return fmt.Errorf("failed to collect buffers: %w", err)
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

			const deleteVisible = `
				DELETE FROM metadata
				WHERE object_id = $1
			`

			if _, err := tx.Exec(ctx, deleteVisible, oid); err != nil {
				return fmt.Errorf("failed to delete old metadata: %w", err)
			}

			const moveMetadata = `
				UPDATE metadata
				SET object_id = $1
				WHERE object_id = $2
			`

			if _, err := tx.Exec(ctx, moveMetadata, oid, soid); err != nil {
				return fmt.Errorf("failed to move metadata: %w", err)
			}

			// The upload has to follow its metadata onto the visible object.
			// Deleting the temporary object below would otherwise cascade into
			// the uploads entry, taking the metadata that was just moved and any
			// pending upload job with it.
			const moveUpload = `
				UPDATE uploads
				SET object_id = $1
				WHERE id = $2
			`

			if _, err := tx.Exec(ctx, moveUpload, oid, uid); err != nil {
				return fmt.Errorf("failed to move upload: %w", err)
			}

			const updateVisible = `
				UPDATE objects
				SET
					account = $2,
					modified_at = NOW(),
					size = COALESCE((
						SELECT SUM(data_length)
						FROM metadata
						WHERE object_id = $1
					), 0)
				WHERE id = $1
			`

			tag, err := tx.Exec(ctx, updateVisible, oid, aid)
			if err != nil {
				return fmt.Errorf("failed to update visible object: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}

			const deleteTemporary = `
				DELETE FROM objects
				WHERE id = $1
			`

			tag, err = tx.Exec(ctx, deleteTemporary, soid)
			if err != nil {
				return fmt.Errorf("failed to delete temporary object: %w", err)
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

			finalOid = oid
		}

		const clearUploadID = `
			UPDATE metadata
			SET upload_id = NULL
			WHERE object_id = $1
				AND upload_id = $2
		`

		if _, err := tx.Exec(ctx, clearUploadID, finalOid, uid); err != nil {
			return fmt.Errorf("failed to clear upload ID from metadata: %w", err)
		}

		return nil
	})
}

// GetMetadata retrieves the metadata of the file at the specified path that
// intersects the given range. The returned slab slices are clipped to the range,
// so that only the requested part of any buffered slab data is fetched from the
// database.
func (db *Database) GetMetadata(acc Account, share, path string, offset, length uint64) (slabs []SlabSlice, err error) {
	path = normalizePath(path)

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
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
			clipped AS (
				SELECT
					m.slab_key,
					m.buffer_id,
					GREATEST(m.obj_offset, $4::BIGINT) AS obj_offset,
					m.data_offset + GREATEST(m.obj_offset, $4::BIGINT) - m.obj_offset AS data_offset,
					LEAST(m.obj_offset + m.data_length, $4::BIGINT + $5::BIGINT) - GREATEST(m.obj_offset, $4::BIGINT) AS data_length
				FROM metadata m
				JOIN target t ON t.id = m.object_id
				WHERE m.obj_offset < $4::BIGINT + $5::BIGINT
					AND m.obj_offset + m.data_length > $4::BIGINT
			)
			SELECT
				c.obj_offset,
				c.slab_key,
				c.data_offset,
				c.data_length,
				SUBSTRING(b.data FROM c.data_offset::INT + 1 FOR c.data_length::INT)
			FROM clipped c
			LEFT JOIN buffers b ON b.id = c.buffer_id
			ORDER BY c.obj_offset
		`

		rows, err := tx.Query(ctx, query, share, path, acc.ID, int64(offset), int64(length))
		if err != nil {
			return fmt.Errorf("failed to query object metadata: %v", err)
		}
		defer rows.Close()

		var found bool
		for rows.Next() {
			found = true

			var (
				objOffset  uint64
				slabKey    []byte
				dataOffset uint64
				dataLength uint64
				chunk      []byte
			)

			if err := rows.Scan(&objOffset, &slabKey, &dataOffset, &dataLength, &chunk); err != nil {
				return fmt.Errorf("failed to scan object metadata: %v", err)
			}

			if slabKey == nil {
				pr := SlabSlice{
					Offset: dataOffset,
					Length: dataLength,
					At:     objOffset,
					Data:   chunk,
				}
				slabs = append(slabs, pr)
				continue
			}

			if len(slabKey) != 32 {
				return fmt.Errorf("invalid key length: %d", len(slabKey))
			}

			var key types.Hash256
			copy(key[:], slabKey)
			slabs = append(slabs, SlabSlice{
				Key:    key,
				Offset: dataOffset,
				Length: dataLength,
				At:     objOffset,
			})
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("failed while reading object metadata: %v", err)
		}

		if found {
			return nil
		}

		return ErrNotFound
	})

	return
}
