package stores

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

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
			no_existing_upload AS (
				SELECT 1
				FROM parent p
				WHERE NOT EXISTS (
					SELECT 1
					FROM uploads u
					JOIN objects o ON o.id = u.object_id
					WHERE o.share_name = $1
						AND o.full_path = $4
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

		const collectBuffers = `
			SELECT DISTINCT m.buffer_id
			FROM metadata m
			WHERE m.object_id = $1
				AND m.buffer_id IS NOT NULL
		`

		rows, err := tx.Query(ctx, collectBuffers, soid)
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

		const collectSlabs = `
			SELECT DISTINCT m.slab_key
			FROM metadata m
			WHERE m.object_id = $1
				AND m.slab_key IS NOT NULL
			ORDER BY m.slab_key
		`

		rows, err = tx.Query(ctx, collectSlabs, soid)
		if err != nil {
			return fmt.Errorf("failed to collect slab keys: %w", err)
		}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return fmt.Errorf("failed to scan slab key: %w", err)
			}
			if len(raw) != 32 {
				rows.Close()
				return fmt.Errorf("invalid slab key length: %d", len(raw))
			}
			var h types.Hash256
			copy(h[:], raw)
			slabs = append(slabs, h)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("failed to iterate slab keys: %w", err)
		}
		rows.Close()

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

		return nil
	})

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
		const cleanupQuery = `
			DELETE FROM upload_jobs uj
			WHERE NOT EXISTS (
				SELECT 1
				FROM metadata m
				JOIN buffers b ON b.id = m.buffer_id
				WHERE m.id = uj.metadata_id
			)
		`

		if _, err := tx.Exec(ctx, cleanupQuery); err != nil {
			return fmt.Errorf("failed to clean up stale upload jobs: %w", err)
		}

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
		job.Data = append([]byte(nil), data[job.DataOffset:end]...)
		return nil
	})
	return
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

// ListSlabs retrieves the slab keys of all files at or down the specified path.
func (db *Database) ListSlabs(acc Account, share, path string) (slabs []types.Hash256, err error) {
	path = normalizePath(path)

	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			WITH caller AS (
				SELECT id, workgroup
				FROM accounts
				WHERE id = $3
			),
			target_file AS (
				SELECT o.id, o.full_path
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
			target_dir AS (
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
			visible_objects AS (
				SELECT o.id
				FROM objects o
				JOIN target_file tf ON tf.id = o.id

				UNION

				SELECT o.id
				FROM objects o
				JOIN accounts owner ON owner.id = o.account
				LEFT JOIN directories od
					ON od.share_name = o.share_name
					AND od.id = o.directory_id
				JOIN target_dir td ON TRUE
				CROSS JOIN caller c
				WHERE o.share_name = $1
					AND o.full_path LIKE td.full_path || '/%%'
					AND o.temporary = FALSE
					AND (
						o.account = c.id
						OR (
							od.private = FALSE
							AND owner.workgroup = c.workgroup
						)
					)
			),
			target_exists AS (
				SELECT EXISTS (SELECT 1 FROM target_file)
					OR EXISTS (SELECT 1 FROM target_dir) AS found
			)
			SELECT
				te.found,
				(
					SELECT COALESCE(
						ARRAY_AGG(DISTINCT m.slab_key),
						'{}'::bytea[]
					)
					FROM visible_objects vo
					JOIN metadata m ON m.object_id = vo.id
					WHERE m.slab_key IS NOT NULL
				) AS slab_keys
			FROM target_exists te

		`

		var found bool
		var rawKeys [][]byte

		if err := tx.QueryRow(ctx, query, share, path, acc.ID).Scan(&found, &rawKeys); err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}

		slabs = make([]types.Hash256, 0, len(rawKeys))
		for _, b := range rawKeys {
			if len(b) != 32 {
				return fmt.Errorf("invalid slab key length: %d", len(b))
			}
			var h types.Hash256
			copy(h[:], b)
			slabs = append(slabs, h)
		}
		return nil
	})

	return
}

// GetMetadata retrieves the metadata of the file at the specified path.
func (db *Database) GetMetadata(acc Account, share, path string) (slabs []SlabSlice, err error) {
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
			)
			SELECT
				m.obj_offset,
				m.slab_key,
				m.data_offset,
				m.data_length,
				b.data
			FROM metadata m
			JOIN target t ON t.id = m.object_id
			LEFT JOIN buffers b ON b.id = m.buffer_id
			ORDER BY m.obj_offset
		`

		rows, err := tx.Query(ctx, query, share, path, acc.ID)
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
					Data:   chunk[dataOffset : dataOffset+dataLength],
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
