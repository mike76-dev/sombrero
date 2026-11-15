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

const (
	// activityPageLimit is the most entries one page of the feed holds.
	activityPageLimit = 200
	// activityGap is how soon after writing a file the same account's next
	// write of it counts as the same editing session, shown as one entry.
	activityGap = 10 * time.Minute
)

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
// real name is how some editors save, and counts as updating that file. The
// saves of an editing session are one entry: an update within the gap of the
// account's last write of the file takes that entry's place, under its action.
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
		if a.Action == ActivityUpdate {
			const session = `
				DELETE FROM activity
				WHERE id = (
					SELECT id FROM activity
					WHERE share_name = $1 AND workgroup = $2 AND username = $3 AND path = $4
						AND action IN ($5, $6) AND at > NOW() - make_interval(secs => $7)
					ORDER BY id DESC LIMIT 1
				)
				RETURNING action
			`
			var action string
			err := tx.QueryRow(ctx, session, a.Share, a.Workgroup, a.Username, a.Path, ActivityUpload, ActivityUpdate, activityGap.Seconds()).Scan(&action)
			if err == nil {
				a.Action = action
			} else if err != pgx.ErrNoRows {
				return fmt.Errorf("failed to find the editing session: %w", err)
			}
		}
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
