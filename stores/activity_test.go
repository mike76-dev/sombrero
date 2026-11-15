package stores

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// record adds an entry to the feed or fails the test.
func record(t *testing.T, db *Database, a Activity) {
	t.Helper()
	if err := db.RecordActivity(a); err != nil {
		t.Fatalf("RecordActivity(%+v): %v", a, err)
	}
}

// TestActivityFeed verifies that a workgroup sees what its accounts did on a
// share, newest first and page by page, and nothing of other workgroups or
// other shares.
func TestActivityFeed(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	ours, theirs := uuid.NewString(), uuid.NewString()
	record(t, db, Activity{Share: "docs", Workgroup: ours, Username: "alice", Action: ActivityUpload, Path: "report.docx", Size: 1200})
	record(t, db, Activity{Share: "docs", Workgroup: ours, Username: "bob", Action: ActivityMkdir, Path: "/Plans"})
	record(t, db, Activity{Share: "docs", Workgroup: ours, Username: "alice", Action: ActivityRename, Path: "report.docx", Target: "Plans/report.docx"})
	record(t, db, Activity{Share: "docs", Workgroup: theirs, Username: "carol", Action: ActivityDelete, Path: "/notes.txt"})
	record(t, db, Activity{Share: "pub", Workgroup: ours, Username: "alice", Action: ActivityUpload, Path: "/logo.png"})

	acc := Account{Username: "alice", Workgroup: ours}
	feed, err := db.Activity("docs", acc, 0, 0)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(feed) != 3 {
		t.Fatalf("want three entries, got %+v", feed)
	}
	if feed[0].Action != ActivityRename || feed[0].Path != "/report.docx" || feed[0].Target != "/Plans/report.docx" || feed[0].Username != "alice" {
		t.Errorf("want the rename first, with the paths made absolute, got %+v", feed[0])
	}
	if feed[1].Action != ActivityMkdir || feed[1].Path != "/Plans" || feed[1].Username != "bob" {
		t.Errorf("want the folder second, got %+v", feed[1])
	}
	if feed[2].Action != ActivityUpload || feed[2].Size != 1200 || time.Since(feed[2].At) > time.Minute {
		t.Errorf("want the upload last, with its size and the time, got %+v", feed[2])
	}

	// A page of two, then the rest from where it ended.
	page, err := db.Activity("docs", acc, 0, 2)
	if err != nil || len(page) != 2 || page[0].ID != feed[0].ID || page[1].ID != feed[1].ID {
		t.Fatalf("want the first two entries, got %+v, %v", page, err)
	}
	rest, err := db.Activity("docs", acc, page[1].ID, 2)
	if err != nil || len(rest) != 1 || rest[0].ID != feed[2].ID {
		t.Fatalf("want the last entry, got %+v, %v", rest, err)
	}

	if feed, err := db.Activity("docs", Account{Username: "carol", Workgroup: theirs}, 0, 0); err != nil || len(feed) != 1 || feed[0].Username != "carol" {
		t.Errorf("want the other workgroup to see its own entry only, got %+v, %v", feed, err)
	}
	if feed, err := db.Activity("other", acc, 0, 0); err != nil || len(feed) != 0 {
		t.Errorf("want nothing on a share with no activity, got %+v, %v", feed, err)
	}
}

// TestActivitySkipsScratchFiles verifies that an editor's scratch files leave
// no entries, and that a scratch file renamed onto a document is recorded as
// an update of the document.
func TestActivitySkipsScratchFiles(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	wg := uuid.NewString()
	record(t, db, Activity{Share: "docs", Workgroup: wg, Username: "alice", Action: ActivityUpload, Path: "/~WRL0001.tmp", Size: 5})
	record(t, db, Activity{Share: "docs", Workgroup: wg, Username: "alice", Action: ActivityRename, Path: "/report.docx", Target: "/report.docx~"})
	record(t, db, Activity{Share: "docs", Workgroup: wg, Username: "alice", Action: ActivityRename, Path: "/~WRL0001.tmp", Target: "/report.docx"})
	record(t, db, Activity{Share: "docs", Workgroup: wg, Username: "alice", Action: ActivityDelete, Path: "/report.docx~"})
	record(t, db, Activity{Share: "docs", Workgroup: wg, Username: "alice", Action: ActivityDelete, Path: "/.~lock.report.docx#"})

	feed, err := db.Activity("docs", Account{Workgroup: wg}, 0, 0)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(feed) != 1 || feed[0].Action != ActivityUpdate || feed[0].Path != "/report.docx" || feed[0].Target != "" {
		t.Errorf("want one update of the document, got %+v", feed)
	}
}

// TestExpireActivity verifies that the entries past the age go and the rest stay.
func TestExpireActivity(t *testing.T) {
	ctx := context.Background()
	db := NewTestStore(t, ctx)
	defer db.Close()

	wg := uuid.NewString()
	record(t, db, Activity{Share: "docs", Workgroup: wg, Username: "alice", Action: ActivityUpload, Path: "/old.txt"})
	record(t, db, Activity{Share: "docs", Workgroup: wg, Username: "alice", Action: ActivityUpload, Path: "/new.txt"})
	err := db.txn(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE activity SET at = NOW() - INTERVAL '100 days' WHERE path = '/old.txt'")
		return err
	})
	if err != nil {
		t.Fatalf("ageing the entry: %v", err)
	}

	if n, err := db.ExpireActivity(90 * 24 * time.Hour); err != nil || n != 1 {
		t.Fatalf("ExpireActivity: want one dropped, got %d, %v", n, err)
	}
	feed, err := db.Activity("docs", Account{Workgroup: wg}, 0, 0)
	if err != nil || len(feed) != 1 || feed[0].Path != "/new.txt" {
		t.Errorf("want the new entry left, got %+v, %v", feed, err)
	}
	if n, err := db.ExpireActivity(0); err != nil || n != 0 {
		t.Errorf("no age: want nothing dropped, got %d, %v", n, err)
	}
}
