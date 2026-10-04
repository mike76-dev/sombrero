package stores

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mike76-dev/sombrero/transfer"
)

// SnapshotStats is what a snapshot came to: what it describes, how much of the
// data it carries in itself, and how many files it could not describe in full.
type SnapshotStats struct {
	Directories int
	Files       int
	Inlined     uint64
	Incomplete  int
}

// Snapshot writes a catalog of a connection: the share, the workgroup with its
// accounts and policies, the app key, and every folder and file of the share
// that belongs to the workgroup, all as of one moment. A buffered piece up to
// inlineCap bytes is carried in the catalog itself, so that its file is whole
// there; a larger one, or one still being written, is left out, which leaves
// its file incomplete.
//
// It is for the rows that nothing but this database holds: what is on the
// network is only pointed at, by object key.
func (db *Database) Snapshot(w io.Writer, share string, workgroup int, inlineCap uint64) (stats SnapshotStats, err error) {
	// One consistent view of a running server, and nothing changed by it.
	tx, err := db.pool.BeginTx(db.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return stats, fmt.Errorf("failed to begin the snapshot: %w", err)
	}
	defer tx.Rollback(db.ctx)

	conn, wgUUID, err := snapshotConnection(db.ctx, tx, share, workgroup)
	if err != nil {
		return stats, err
	}

	tw, err := transfer.NewWriter(w, transfer.Header{
		CreatedAt: time.Now().UTC(),
		Source:    "sombrero",
		Share:     share,
		Workgroup: wgUUID.String(),
	})
	if err != nil {
		return stats, err
	}
	if err := tw.Connection(conn); err != nil {
		return stats, err
	}

	if stats.Directories, err = snapshotDirectories(db.ctx, tx, tw, share, workgroup); err != nil {
		return stats, err
	}
	if err := snapshotFiles(db.ctx, tx, tw, share, workgroup, inlineCap, &stats); err != nil {
		return stats, err
	}

	return stats, tw.Close()
}

// ServerStats is what a catalog of the server came to.
type ServerStats struct {
	Shares     int
	Workgroups int
	Accounts   int
	Bans       int
}

// SnapshotServer writes a catalog of what belongs to no one connection: every
// share as registered, every workgroup with its public folders and accounts, and
// the bans. The anonymous identity is left out, since the server makes it anew.
func (db *Database) SnapshotServer(w io.Writer) (stats ServerStats, err error) {
	tx, err := db.pool.BeginTx(db.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return stats, fmt.Errorf("failed to begin the snapshot: %w", err)
	}
	defer tx.Rollback(db.ctx)
	ctx := db.ctx

	var server transfer.Server

	rows, err := tx.Query(ctx, `SELECT `+shareColumns+` FROM shares s ORDER BY s.share_name`)
	if err != nil {
		return stats, fmt.Errorf("failed to read the shares: %w", err)
	}
	shares, err := scanShares(rows)
	if err != nil {
		return stats, err
	}
	for _, s := range shares {
		server.Shares = append(server.Shares, transfer.Share{
			Name: s.Name, Type: s.Type, Server: s.ServerName, Password: s.Password, Bucket: s.Bucket, Remark: s.Remark,
			CreatedAt: s.CreatedAt.UTC(), DataShards: s.DataShards, ParityShards: s.ParityShards,
			AllowGuest: s.AllowGuest, AllowAnonymous: s.AllowAnonymous, PublicDir: s.PublicDir,
		})
	}

	const workgroups = `SELECT id, uuid, name FROM workgroups WHERE uuid <> $1 ORDER BY id`
	rows, err = tx.Query(ctx, workgroups, AnonymousWorkgroup[:])
	if err != nil {
		return stats, fmt.Errorf("failed to read the workgroups: %w", err)
	}
	type group struct {
		id int
		wa *transfer.WorkgroupAccounts
	}
	var groups []group
	byID := make(map[int]*transfer.WorkgroupAccounts)
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id int
		var u []byte
		var name *string
		if err := row.Scan(&id, &u, &name); err != nil {
			return struct{}{}, err
		}
		wa := &transfer.WorkgroupAccounts{}
		copy(wa.Workgroup.UUID[:], u)
		if name != nil {
			wa.Workgroup.Name = *name
		}
		groups = append(groups, group{id: id, wa: wa})
		byID[id] = wa
		return struct{}{}, nil
	})
	if err != nil {
		return stats, fmt.Errorf("failed to read the workgroups: %w", err)
	}

	rows, err = tx.Query(ctx, `SELECT workgroup, path, read_only, case_sensitive FROM public_dirs ORDER BY workgroup, id`)
	if err != nil {
		return stats, fmt.Errorf("failed to read the public folders: %w", err)
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id int
		var pd transfer.PublicDir
		if err := row.Scan(&id, &pd.Path, &pd.ReadOnly, &pd.CaseSensitive); err != nil {
			return struct{}{}, err
		}
		if wa, ok := byID[id]; ok {
			wa.Workgroup.PublicDirs = append(wa.Workgroup.PublicDirs, pd)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return stats, fmt.Errorf("failed to read the public folders: %w", err)
	}

	rows, err = tx.Query(ctx, `SELECT workgroup, account_name, password_hash, created_at FROM accounts ORDER BY workgroup, id`)
	if err != nil {
		return stats, fmt.Errorf("failed to read the accounts: %w", err)
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id int
		var acc transfer.Account
		if err := row.Scan(&id, &acc.Name, &acc.PasswordHash, &acc.CreatedAt); err != nil {
			return struct{}{}, err
		}
		if wa, ok := byID[id]; ok {
			acc.CreatedAt = acc.CreatedAt.UTC()
			wa.Accounts = append(wa.Accounts, acc)
			stats.Accounts++
		}
		return struct{}{}, nil
	})
	if err != nil {
		return stats, fmt.Errorf("failed to read the accounts: %w", err)
	}
	for _, g := range groups {
		server.Workgroups = append(server.Workgroups, *g.wa)
	}

	rows, err = tx.Query(ctx, `SELECT host, reason FROM bans ORDER BY host`)
	if err != nil {
		return stats, fmt.Errorf("failed to read the bans: %w", err)
	}
	server.Bans, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (transfer.Ban, error) {
		var b transfer.Ban
		return b, row.Scan(&b.Host, &b.Reason)
	})
	if err != nil {
		return stats, fmt.Errorf("failed to read the bans: %w", err)
	}

	stats.Shares, stats.Workgroups, stats.Bans = len(server.Shares), len(server.Workgroups), len(server.Bans)

	tw, err := transfer.NewWriter(w, transfer.Header{CreatedAt: time.Now().UTC(), Source: "sombrero"})
	if err != nil {
		return stats, err
	}
	if err := tw.Server(server); err != nil {
		return stats, err
	}

	return stats, tw.Close()
}

// snapshotConnection reads what the folders and files belong to.
func snapshotConnection(ctx context.Context, tx pgx.Tx, share string, workgroup int) (transfer.Connection, uuid.UUID, error) {
	const query = `
		SELECT s.share_type, s.server_name, s.api_password, s.bucket, s.remark, s.created_at,
			s.data_shards, s.parity_shards, s.allow_guest, s.allow_anonymous, s.public_dir,
			w.uuid, w.name, c.app_key
		FROM connections c
		JOIN shares s ON s.share_name = c.share_name
		JOIN workgroups w ON w.id = c.workgroup
		WHERE c.share_name = $1 AND c.workgroup = $2
	`
	conn := transfer.Connection{Share: transfer.Share{Name: share}}
	var (
		shards       [2]int
		wgUUID       []byte
		wgName       *string
		appKey       []byte
		shareCreated time.Time
	)
	err := tx.QueryRow(ctx, query, share, workgroup).Scan(
		&conn.Share.Type, &conn.Share.Server, &conn.Share.Password, &conn.Share.Bucket, &conn.Share.Remark, &shareCreated,
		&shards[0], &shards[1], &conn.Share.AllowGuest, &conn.Share.AllowAnonymous, &conn.Share.PublicDir,
		&wgUUID, &wgName, &appKey,
	)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
		return conn, uuid.UUID{}, fmt.Errorf("%w: the connection of workgroup %d to %q", ErrNotFound, workgroup, share)
	}
	if err != nil {
		return conn, uuid.UUID{}, fmt.Errorf("failed to read the connection: %w", err)
	}
	conn.Share.CreatedAt = shareCreated.UTC()
	conn.Share.DataShards, conn.Share.ParityShards = uint8(shards[0]), uint8(shards[1])
	conn.AppKey = appKey
	if wgName != nil {
		conn.Workgroup.Name = *wgName
	}
	u, err := uuid.FromBytes(wgUUID)
	if err != nil {
		return conn, uuid.UUID{}, fmt.Errorf("the workgroup has no identity: %w", err)
	}
	conn.Workgroup.UUID = u

	rows, err := tx.Query(ctx, `SELECT path, read_only, case_sensitive FROM public_dirs WHERE workgroup = $1 ORDER BY id`, workgroup)
	if err != nil {
		return conn, u, fmt.Errorf("failed to read the public folders: %w", err)
	}
	conn.Workgroup.PublicDirs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (transfer.PublicDir, error) {
		var pd transfer.PublicDir
		return pd, row.Scan(&pd.Path, &pd.ReadOnly, &pd.CaseSensitive)
	})
	if err != nil {
		return conn, u, fmt.Errorf("failed to read the public folders: %w", err)
	}

	rows, err = tx.Query(ctx, `SELECT account_name, password_hash, created_at FROM accounts WHERE workgroup = $1 ORDER BY id`, workgroup)
	if err != nil {
		return conn, u, fmt.Errorf("failed to read the accounts: %w", err)
	}
	conn.Accounts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (transfer.Account, error) {
		var acc transfer.Account
		err := row.Scan(&acc.Name, &acc.PasswordHash, &acc.CreatedAt)
		acc.CreatedAt = acc.CreatedAt.UTC()
		return acc, err
	})
	if err != nil {
		return conn, u, fmt.Errorf("failed to read the accounts: %w", err)
	}

	const policies = `
		SELECT a.account_name, p.read_access, p.write_access, p.delete_access, p.execute_access
		FROM policies p
		JOIN accounts a ON a.id = p.account
		WHERE p.share_name = $1 AND p.workgroup = $2
		ORDER BY a.id
	`
	rows, err = tx.Query(ctx, policies, share, workgroup)
	if err != nil {
		return conn, u, fmt.Errorf("failed to read the policies: %w", err)
	}
	conn.Policies, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (transfer.Policy, error) {
		var p transfer.Policy
		return p, row.Scan(&p.Account, &p.Read, &p.Write, &p.Delete, &p.Execute)
	})
	if err != nil {
		return conn, u, fmt.Errorf("failed to read the policies: %w", err)
	}

	return conn, u, nil
}

// snapshotDirectories writes the folders of the share that belong to the
// workgroup, parents before children, which is the order of their paths.
func snapshotDirectories(ctx context.Context, tx pgx.Tx, tw *transfer.Writer, share string, workgroup int) (int, error) {
	const query = `
		SELECT d.full_path, a.account_name, d.private, d.read_only, d.created_at, d.modified_at
		FROM directories d
		JOIN accounts a ON a.id = d.account
		WHERE d.share_name = $1 AND d.workgroup = $2
		ORDER BY d.full_path
	`
	rows, err := tx.Query(ctx, query, share, workgroup)
	if err != nil {
		return 0, fmt.Errorf("failed to read the folders: %w", err)
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var dir transfer.Directory
		if err := rows.Scan(&dir.Path, &dir.Owner, &dir.Private, &dir.ReadOnly, &dir.CreatedAt, &dir.ModifiedAt); err != nil {
			return count, fmt.Errorf("failed to scan a folder: %w", err)
		}
		dir.CreatedAt, dir.ModifiedAt = dir.CreatedAt.UTC(), dir.ModifiedAt.UTC()
		if err := tw.Directory(dir); err != nil {
			return count, err
		}
		count++
	}

	return count, rows.Err()
}

// snapshotFiles writes the files of the share that belong to the workgroup, each
// with the runs of it that are on the network and the buffered ones small enough
// to carry.
func snapshotFiles(ctx context.Context, tx pgx.Tx, tw *transfer.Writer, share string, workgroup int, inlineCap uint64, stats *SnapshotStats) error {
	// An upload still in flight is left out of a catalog, the way the packer
	// leaves it out of a slab: it may yet be abandoned.
	const query = `
		SELECT o.id, o.full_path, a.account_name, o.size, o.created_at, o.modified_at,
			m.obj_offset, m.slab_key, m.data_offset, m.data_length,
			CASE WHEN m.buffer_id IS NOT NULL AND m.upload_id IS NULL AND m.data_length <= $3::BIGINT
				THEN SUBSTRING(b.data FROM m.data_offset::INT + 1 FOR m.data_length::INT)
			END
		FROM objects o
		JOIN accounts a ON a.id = o.account
		LEFT JOIN metadata m ON m.object_id = o.id
		LEFT JOIN buffers b ON b.id = m.buffer_id
		WHERE o.share_name = $1 AND o.workgroup = $2 AND o.temporary = FALSE
		ORDER BY o.id, m.obj_offset
	`
	rows, err := tx.Query(ctx, query, share, workgroup, int64(inlineCap))
	if err != nil {
		return fmt.Errorf("failed to read the files: %w", err)
	}
	defer rows.Close()

	flush := func(file *transfer.File) error {
		if file == nil {
			return nil
		}
		if err := tw.File(*file); err != nil {
			return err
		}
		stats.Files++
		if !file.Complete() {
			stats.Incomplete++
		}
		return nil
	}

	var current *transfer.File
	var currentID uint64
	for rows.Next() {
		var (
			id                    uint64
			file                  transfer.File
			objOffset, dataOffset *int64
			dataLength            *int64
			slabKey, inline       []byte
			createdAt, modifiedAt time.Time
		)
		if err := rows.Scan(&id, &file.Path, &file.Owner, &file.Size, &createdAt, &modifiedAt,
			&objOffset, &slabKey, &dataOffset, &dataLength, &inline); err != nil {
			return fmt.Errorf("failed to scan a file: %w", err)
		}
		file.CreatedAt, file.ModifiedAt = createdAt.UTC(), modifiedAt.UTC()

		if current == nil || currentID != id {
			if err := flush(current); err != nil {
				return err
			}
			current, currentID = &file, id
		}

		// An empty file has no rows of metadata, and a piece that is on the
		// network or carried here is a part; anything else is a gap.
		if objOffset == nil {
			continue
		}
		part := transfer.Part{Offset: uint64(*objOffset), DataOffset: uint64(*dataOffset), Length: uint64(*dataLength)}
		switch {
		case slabKey != nil:
			copy(part.Object[:], slabKey)
		case inline != nil:
			part.Inline = inline
			part.DataOffset = 0
			stats.Inlined += part.Length
		default:
			continue
		}
		current.Parts = append(current.Parts, part)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read the files: %w", err)
	}

	return flush(current)
}
