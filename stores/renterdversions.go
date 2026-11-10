package stores

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RenterdVersion is a version of a file on a renterd share, which the bucket
// holds as a hidden object named after the ID, under the path the file goes by.
type RenterdVersion struct {
	Version
	Path string
}

// RenterdReplacement is a file on a renterd share about to lose its contents:
// to a save, or deliberately, to a restore or a deletion.
type RenterdReplacement struct {
	Share      string
	Path       string
	Account    Account
	Size       uint64
	ModifiedAt time.Time
	Deliberate bool
}

// renterdVersionColumns are the columns a Version is read from.
const renterdVersionColumns = `
	v.id, v.size, v.modified_at, v.replaced_at, COALESCE(a.account_name, ''), v.kept
	FROM renterd_versions v
	LEFT JOIN accounts a ON a.id = v.replaced_by
`

// renterdPrefix is the prefix of the paths under the folder, the root included.
func renterdPrefix(dir string) string {
	return strings.TrimSuffix(normalizePath(dir), "/") + "/"
}

// KeepRenterdFile decides whether the contents the file is losing are kept as
// a version, as emptyFile does for an indexd file, and records it where they
// are: its ID, zero where they go, and the versions the count pruned.
func (db *Database) KeepRenterdFile(r RenterdReplacement) (id uint64, pruned []uint64, err error) {
	path := normalizePath(r.Path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		keep, err := db.keepsRenterdReplaced(ctx, tx, r.Share, path, uint64(r.Account.ID), r.Deliberate)
		if err != nil {
			return err
		}
		// Whatever the file held, it is about to hold something else.
		if _, err := tx.Exec(ctx, `DELETE FROM renterd_restored WHERE share_name = $1 AND path = $2`, r.Share, path); err != nil {
			return fmt.Errorf("failed to forget the restored version: %w", err)
		}
		if !keep {
			return nil
		}
		if id, err = recordRenterdVersion(ctx, tx, r.Share, path, r.Size, r.ModifiedAt, uint64(r.Account.ID)); err != nil {
			return err
		}
		pruned, err = db.pruneRenterdVersions(ctx, tx, r.Share, path)
		return err
	})
	return
}

// keepsRenterdReplaced is keepsReplaced for a renterd file: a deliberate
// replacement keeps the contents unless they are a restored version untouched.
func (db *Database) keepsRenterdReplaced(ctx context.Context, tx pgx.Tx, share, path string, account uint64, deliberate bool) (bool, error) {
	if !deliberate || !db.versioning.Keep {
		return db.keepsRenterdVersion(ctx, tx, share, path, account)
	}

	var restored bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM renterd_restored WHERE share_name = $1 AND path = $2)`, share, path).Scan(&restored)
	if err != nil {
		return false, fmt.Errorf("failed to look up what the file was restored from: %w", err)
	}
	return !restored, nil
}

// keepsRenterdVersion is keepsVersion for a renterd file: the policy must keep
// versions, and the save must not continue the session of the newest one.
func (db *Database) keepsRenterdVersion(ctx context.Context, tx pgx.Tx, share, path string, account uint64) (bool, error) {
	p := db.versioning
	if !p.Keep {
		return false, nil
	}
	if p.Gap <= 0 {
		return true, nil
	}

	const query = `
		SELECT id,
			COALESCE(replaced_by = $3, FALSE)
			AND session_until > NOW() - make_interval(secs => $4)
			AND ($5 <= 0 OR replaced_at > NOW() - make_interval(secs => $5))
		FROM renterd_versions
		WHERE share_name = $1
			AND path = $2
		ORDER BY replaced_at DESC, id DESC
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
	if _, err := tx.Exec(ctx, `UPDATE renterd_versions SET session_until = NOW() WHERE id = $1`, vid); err != nil {
		return false, fmt.Errorf("failed to extend the editing session: %w", err)
	}
	return false, nil
}

// recordRenterdVersion records the contents of the file as a version replaced
// by the account, and returns its ID, which names the object holding them.
func recordRenterdVersion(ctx context.Context, tx pgx.Tx, share, path string, size uint64, modified time.Time, account uint64) (uint64, error) {
	const record = `
		INSERT INTO renterd_versions (share_name, path, size, modified_at, replaced_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`

	var id uint64
	if err := tx.QueryRow(ctx, record, share, path, int64(size), modified, account).Scan(&id); err != nil {
		return 0, fmt.Errorf("failed to record the version: %w", err)
	}
	return id, nil
}

// pruneRenterdVersions drops the versions of the file beyond the count the
// policy keeps, oldest first, the kept ones neither counted nor dropped, and
// returns the IDs that went.
func (db *Database) pruneRenterdVersions(ctx context.Context, tx pgx.Tx, share, path string) ([]uint64, error) {
	count := db.versioning.Count
	if count <= 0 {
		count = DefaultVersionCount
	}

	const extra = `
		DELETE FROM renterd_versions
		WHERE id IN (
			SELECT id
			FROM renterd_versions
			WHERE share_name = $1
				AND path = $2
				AND NOT kept
			ORDER BY replaced_at DESC, id DESC
			OFFSET $3
		)
		RETURNING id
	`

	rows, err := tx.Query(ctx, extra, share, path, count)
	if err != nil {
		return nil, fmt.Errorf("failed to drop the versions beyond the count: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[uint64])
}

// ForgetRenterdVersion drops the record of a version whose contents could not
// be moved out of the way after all.
func (db *Database) ForgetRenterdVersion(id uint64) error {
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM renterd_versions WHERE id = $1`, int64(id))
		return err
	})
}

// RenterdVersions lists the versions of the file at the path, newest first.
func (db *Database) RenterdVersions(share, path string) (versions []Version, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `SELECT ` + renterdVersionColumns + `
			WHERE v.share_name = $1
				AND v.path = $2
			ORDER BY v.replaced_at DESC, v.id DESC
		`

		rows, err := tx.Query(ctx, query, share, path)
		if err != nil {
			return fmt.Errorf("failed to list the versions: %w", err)
		}
		versions, err = pgx.CollectRows(rows, scanVersion)
		return err
	})
	return
}

// RenterdVersion is the version of the file at the path with the ID, or
// ErrNotFound: a version of another file is not reachable under the path.
func (db *Database) RenterdVersion(share, path string, id uint64) (version Version, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `SELECT ` + renterdVersionColumns + `
			WHERE v.share_name = $1
				AND v.path = $2
				AND v.id = $3
		`

		rows, err := tx.Query(ctx, query, share, path, int64(id))
		if err != nil {
			return fmt.Errorf("failed to look up the version: %w", err)
		}
		version, err = pgx.CollectExactlyOneRow(rows, scanVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return
}

// scanVersion reads a Version from the renterdVersionColumns.
func scanVersion(row pgx.CollectableRow) (v Version, err error) {
	err = row.Scan(&v.ID, &v.Size, &v.ModifiedAt, &v.ReplacedAt, &v.ReplacedBy, &v.Kept)
	return
}

// RenterdVersionsUnder lists the versions of the files under the folder, at
// any depth, with their paths, oldest first within a path.
func (db *Database) RenterdVersionsUnder(share, dir string) (versions []RenterdVersion, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `SELECT v.path, ` + renterdVersionColumns + `
			WHERE v.share_name = $1
				AND starts_with(v.path, $2)
			ORDER BY v.path, v.replaced_at, v.id
		`

		rows, err := tx.Query(ctx, query, share, renterdPrefix(dir))
		if err != nil {
			return fmt.Errorf("failed to list the versions: %w", err)
		}
		versions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (v RenterdVersion, err error) {
			err = row.Scan(&v.Path, &v.ID, &v.Size, &v.ModifiedAt, &v.ReplacedAt, &v.ReplacedBy, &v.Kept)
			return
		})
		return err
	})
	return
}

// RenterdSnapshots lists the times the folder has previous states at, newest
// first: those its versions, at any depth, were last modified at.
func (db *Database) RenterdSnapshots(share, dir string) (times []time.Time, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT DISTINCT date_trunc('second', modified_at) AS at
			FROM renterd_versions
			WHERE share_name = $1
				AND starts_with(path, $2)
			ORDER BY at DESC
		`

		rows, err := tx.Query(ctx, query, share, renterdPrefix(dir))
		if err != nil {
			return fmt.Errorf("failed to list the snapshots: %w", err)
		}
		times, err = pgx.CollectRows(rows, pgx.RowTo[time.Time])
		return err
	})
	return
}

// DeleteRenterdVersions drops the version of the file at the path with the ID,
// or every version of it with a zero ID, kept or not, and returns the IDs.
func (db *Database) DeleteRenterdVersions(share, path string, id uint64) (dropped []uint64, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const remove = `
			DELETE FROM renterd_versions
			WHERE share_name = $1
				AND path = $2
				AND ($3 = 0 OR id = $3)
			RETURNING id
		`

		rows, err := tx.Query(ctx, remove, share, path, int64(id))
		if err != nil {
			return fmt.Errorf("failed to drop the versions: %w", err)
		}
		dropped, err = pgx.CollectRows(rows, pgx.RowTo[uint64])
		return err
	})
	return
}

// DeleteRenterdVersionsUnder drops the versions of every file under the
// folder, as the folder goes, and returns the IDs.
func (db *Database) DeleteRenterdVersionsUnder(share, dir string) (dropped []uint64, err error) {
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const remove = `
			DELETE FROM renterd_versions
			WHERE share_name = $1
				AND starts_with(path, $2)
			RETURNING id
		`

		rows, err := tx.Query(ctx, remove, share, renterdPrefix(dir))
		if err != nil {
			return fmt.Errorf("failed to drop the versions: %w", err)
		}
		dropped, err = pgx.CollectRows(rows, pgx.RowTo[uint64])
		return err
	})
	return
}

// KeepRenterdVersion exempts the version of the file at the path with the ID
// from the count and age limits, or puts it back under them; none: ErrNotFound.
func (db *Database) KeepRenterdVersion(share, path string, id uint64, keep bool) error {
	path = normalizePath(path)
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			UPDATE renterd_versions
			SET kept = $4
			WHERE share_name = $1
				AND path = $2
				AND id = $3
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

// ExpireRenterdVersions drops the versions of the share replaced longer ago
// than the age, the kept ones excepted, and returns the IDs. A zero age keeps
// them for good.
func (db *Database) ExpireRenterdVersions(share string, age time.Duration) (dropped []uint64, err error) {
	if age <= 0 {
		return nil, nil
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const expired = `
			DELETE FROM renterd_versions
			WHERE share_name = $1
				AND replaced_at < NOW() - make_interval(secs => $2)
				AND NOT kept
			RETURNING id
		`

		rows, err := tx.Query(ctx, expired, share, age.Seconds())
		if err != nil {
			return fmt.Errorf("failed to drop the expired versions: %w", err)
		}
		dropped, err = pgx.CollectRows(rows, pgx.RowTo[uint64])
		return err
	})
	return
}

// MarkRenterdRestored notes that the file, of the size, holds unchanged the
// one of the versions that is of its size; none: ErrNotFound. A zero ID names
// the newest version, as it does for MarkRestored.
func (db *Database) MarkRenterdRestored(share, path string, size uint64, ids []uint64) error {
	path = normalizePath(path)
	candidates := make([]int64, 0, len(ids))
	for _, id := range ids {
		candidates = append(candidates, int64(id))
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const note = `
			INSERT INTO renterd_restored (share_name, path, version_id)
			SELECT $1, $2, v.id
			FROM renterd_versions v
			WHERE v.share_name = $1
				AND v.path = $2
				AND v.size = $3
				AND (v.id = ANY($4::BIGINT[]) OR 0 = ANY($4::BIGINT[]) AND v.replaced_at = (
					SELECT MAX(n.replaced_at)
					FROM renterd_versions n
					WHERE n.share_name = $1
						AND n.path = $2
				))
			ORDER BY v.replaced_at DESC, v.id DESC
			LIMIT 1
			ON CONFLICT (share_name, path) DO UPDATE
			SET version_id = EXCLUDED.version_id
		`

		tag, err := tx.Exec(ctx, note, share, path, int64(size), candidates)
		if err != nil {
			return fmt.Errorf("failed to note the restored version: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RenterdFileRenamed moves the versions of the file, or of the files under the
// folder, to where they now are. A renamed file is noted as displaced, so that
// deleting it soon after counts as replacing the file that took its name.
func (db *Database) RenterdFileRenamed(share, from, to string, isDir bool) error {
	from, to = normalizePath(from), normalizePath(to)
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		if isDir {
			prefix, dest := renterdPrefix(from), renterdPrefix(to)
			for _, table := range []string{"renterd_versions", "renterd_restored"} {
				move := `UPDATE ` + table + ` SET path = $3 || substr(path, length($2) + 1) WHERE share_name = $1 AND starts_with(path, $2)`
				if _, err := tx.Exec(ctx, move, share, prefix, dest); err != nil {
					return fmt.Errorf("failed to move the versions: %w", err)
				}
			}
			_, err := tx.Exec(ctx, `DELETE FROM renterd_displaced WHERE share_name = $1 AND starts_with(path, $2)`, share, prefix)
			return err
		}

		for _, table := range []string{"renterd_versions", "renterd_restored"} {
			move := `UPDATE ` + table + ` SET path = $3 WHERE share_name = $1 AND path = $2`
			if _, err := tx.Exec(ctx, move, share, from, to); err != nil {
				return fmt.Errorf("failed to move the versions: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM renterd_displaced WHERE share_name = $1 AND path = $2`, share, from); err != nil {
			return fmt.Errorf("failed to forget the file's old name: %w", err)
		}
		if !db.versioning.Keep {
			return nil
		}

		const displace = `
			INSERT INTO renterd_displaced (share_name, path, origin)
			VALUES ($1, $2, $3)
			ON CONFLICT (share_name, path) DO UPDATE
			SET origin = EXCLUDED.origin, at = NOW()
		`

		if _, err := tx.Exec(ctx, displace, share, to, from); err != nil {
			return fmt.Errorf("failed to note the file's old name: %w", err)
		}
		return nil
	})
}

// RenterdDisplacedOrigin returns the name the file at the path was renamed away
// from within the displacedWindow, or nothing. Whether another file has taken
// the name since is for the caller to see.
func (db *Database) RenterdDisplacedOrigin(share, path string) (origin string, err error) {
	path = normalizePath(path)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT origin
			FROM renterd_displaced
			WHERE share_name = $1
				AND path = $2
				AND at > NOW() - make_interval(secs => $3)
		`

		err := tx.QueryRow(ctx, query, share, path, displacedWindow.Seconds()).Scan(&origin)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("failed to look up the file's old name: %w", err)
		}
		return nil
	})
	return
}

// AbsorbRenterdFile makes the file at the path, which the caller deletes, the
// previous contents of the file at the origin it was renamed away from: its
// versions move there, and it is kept as one where the session rule says, as
// KeepRenterdFile reports it.
func (db *Database) AbsorbRenterdFile(r RenterdReplacement, origin string) (id uint64, pruned []uint64, err error) {
	path, origin := normalizePath(r.Path), normalizePath(origin)
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE renterd_versions SET path = $3 WHERE share_name = $1 AND path = $2`, r.Share, path, origin); err != nil {
			return fmt.Errorf("failed to move the versions: %w", err)
		}
		for _, table := range []string{"renterd_restored", "renterd_displaced"} {
			if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE share_name = $1 AND path = $2`, r.Share, path); err != nil {
				return fmt.Errorf("failed to forget the file: %w", err)
			}
		}
		keep, err := db.keepsRenterdVersion(ctx, tx, r.Share, origin, uint64(r.Account.ID))
		if err != nil || !keep {
			return err
		}
		if id, err = recordRenterdVersion(ctx, tx, r.Share, origin, r.Size, r.ModifiedAt, uint64(r.Account.ID)); err != nil {
			return err
		}
		pruned, err = db.pruneRenterdVersions(ctx, tx, r.Share, origin)
		return err
	})
	return
}
