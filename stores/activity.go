package stores

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The actions an activity records.
const (
	ActivityUpload  = "upload"
	ActivityUpdate  = "update"
	ActivityDelete  = "delete"
	ActivityRename  = "rename"
	ActivityMkdir   = "mkdir"
	ActivityRmdir   = "rmdir"
	ActivityRestore = "restore"
)

// activityPageLimit is the most entries one page of the feed holds.
const activityPageLimit = 200

// Activity is something an account did on a share: the feed of its workgroup
// is made of these. Target is the new path of a rename, Size that of an upload.
type Activity struct {
	ID        uint64    `json:"id"`
	Share     string    `json:"share"`
	Workgroup string    `json:"workgroup"`
	Username  string    `json:"username"`
	Action    string    `json:"action"`
	Path      string    `json:"path"`
	Target    string    `json:"target,omitempty"`
	Size      uint64    `json:"size,omitempty"`
	At        time.Time `json:"at"`
}

// RecordActivity adds an entry to the feed. An editor's scratch file is nobody's
// business, so nothing is recorded about one; a scratch file renamed onto a
// real name is how some editors save, and counts as updating that file.
func (db *Database) RecordActivity(a Activity) error {
	a.Path = normalizePath(a.Path)
	if a.Target != "" {
		a.Target = normalizePath(a.Target)
	}
	if temporaryName(a.Path) {
		if a.Action != ActivityRename || temporaryName(a.Target) {
			return nil
		}
		a.Action, a.Path, a.Target = ActivityUpdate, a.Target, ""
	} else if a.Action == ActivityRename && temporaryName(a.Target) {
		return nil
	}
	return db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const insert = `
			INSERT INTO activity (share_name, workgroup, username, action, path, target, size)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`
		if _, err := tx.Exec(ctx, insert, a.Share, a.Workgroup, a.Username, a.Action, a.Path, a.Target, a.Size); err != nil {
			return fmt.Errorf("failed to record the activity: %w", err)
		}
		return nil
	})
}

// Activity lists what the accounts of the account's workgroup did on the
// share, newest first: the entries before the one with the ID, or the newest
// where it is zero, as many as the limit, capped, allows.
func (db *Database) Activity(share string, acc Account, before uint64, limit int) (entries []Activity, err error) {
	if limit <= 0 || limit > activityPageLimit {
		limit = activityPageLimit
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		const query = `
			SELECT id, share_name, workgroup, username, action, path, target, size, at
			FROM activity
			WHERE share_name = $1 AND workgroup = $2 AND ($3 = 0 OR id < $3)
			ORDER BY id DESC
			LIMIT $4
		`
		rows, err := tx.Query(ctx, query, share, acc.Workgroup, before, limit)
		if err != nil {
			return fmt.Errorf("failed to list the activity: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a Activity
			if err := rows.Scan(&a.ID, &a.Share, &a.Workgroup, &a.Username, &a.Action, &a.Path, &a.Target, &a.Size, &a.At); err != nil {
				return fmt.Errorf("failed to scan the activity: %w", err)
			}
			entries = append(entries, a)
		}
		return rows.Err()
	})
	return
}

// ExpireActivity drops the entries older than the age, and returns how many went.
func (db *Database) ExpireActivity(age time.Duration) (dropped int, err error) {
	if age <= 0 {
		return 0, nil
	}
	err = db.txn(func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM activity WHERE at < NOW() - make_interval(secs => $1)", age.Seconds())
		if err != nil {
			return fmt.Errorf("failed to expire the activity: %w", err)
		}
		dropped = int(tag.RowsAffected())
		return nil
	})
	return
}

// Activity implements Store: the Lite mode keeps no feed.
func (js *JSONStore) Activity(string, Account, uint64, int) ([]Activity, error) {
	return nil, nil
}
