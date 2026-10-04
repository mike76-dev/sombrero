package client

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
)

// RecoveredFolder is where the files found inside the objects are put, under the
// folder the objects themselves are in.
const RecoveredFolder = "recovered"

// maxCarveSize is the largest object the sorter looks inside, which it holds
// whole while it does. A slab is smaller than this at any redundancy anyone
// serves a share with.
const maxCarveSize = 256 << 20

// defaultSortBatch is how many files a sort looks at where the caller names no
// number. Each one is downloaded whole, so a batch is kept short enough to be
// waited for.
const defaultSortBatch = 8

// sortRoundBudget is how long a round goes on taking up new files for. Slabs come
// slowly from some hosts, and whoever asked is holding a connection open: a round
// that has taken this long stops after the file in hand and says there is more.
var sortRoundBudget = 20 * time.Second

// SortReport is what one round of sorting out the lost and found came to: how
// many objects were looked inside, what was found in them, and how much of them
// is still there, belonging to no file anything could recognize.
//
// Unread counts the objects that could not be downloaded, and Failure says why
// the first of them could not: one slab a host will not hand over is no reason
// to leave the rest unsorted.
//
// Last is the path of the file the round stopped at, which the next round is
// given to take up where this one left off, and More says there was one.
type SortReport struct {
	Objects   int
	Recovered int
	Skipped   int
	Unread    int
	Failure   string
	Bytes     uint64
	Leftover  uint64
	Last      string
	More      bool
}

// SortLostAndFound looks inside the files under the prefix for files that can be
// recognized by their structure, and makes each one a file of its own under the
// recovered folder.
//
// Nothing is uploaded and nothing is paid for twice: a found file is a run of the
// object that is already there, named after where it was found. What was found is
// cut out of the file it was found in, which comes to hold only what nothing
// recognized and goes away once nothing is left: the lost and found is what is
// still unaccounted for, and sorting it again looks only at that.
//
// One round looks at no more than limit of the files that come after the given
// path, since each of them is downloaded whole. What it reports is where it got
// to, for the next round to go on from.
//
// It is for the data whose names are gone. What comes of it are files with the
// right contents and no names of their own, which is as much as the bytes can
// say by themselves.
func (ic *IndexdClient) SortLostAndFound(ctx context.Context, acc stores.Account, prefix, after string, limit int) (SortReport, error) {
	if prefix == "" {
		prefix = LostAndFound
	}
	if limit <= 0 {
		limit = defaultSortBatch
	}

	var report SortReport
	entries, err := ic.db.ListObjects(acc, ic.share, prefix)
	if err != nil {
		return report, fmt.Errorf("couldn't list %s: %w", prefix, err)
	}

	target := stores.TransferTarget{Share: ic.share, Workgroup: ic.workgroup, Owner: acc}
	start := time.Now()
	var looked int
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if entry.IsDir || entry.Path <= after {
			continue
		}
		if looked >= limit || (looked > 0 && time.Since(start) > sortRoundBudget) {
			// There is more to look at, which the caller asks for in a round of
			// its own rather than waiting for all of it in this one.
			report.More = true
			break
		}

		looked++
		report.Last = entry.Path

		key, runs, ok := objectRuns(ic.db, acc, ic.share, entry)
		if !ok {
			report.Skipped++
			continue
		}

		found, leftover, err := ic.sortObject(ctx, target, entry, key, runs, prefix)
		if err != nil {
			if ctx.Err() != nil {
				return report, err
			}
			log.Printf("failed to sort out %s: %v", entry.Path, err)
			report.Unread++
			if report.Failure == "" {
				report.Failure = err.Error()
			}
			continue
		}

		report.Objects++
		report.Leftover += leftover
		for _, find := range found {
			report.Recovered++
			report.Bytes += find.Length
		}
	}

	return report, nil
}

// sortObject looks inside one object, run by run, makes a file of everything it
// finds and cuts it out of the file it was found in. A file cannot span two runs:
// what lies between them was found before. The finds are returned at their
// offsets in the object, and with them what is left of the file.
func (ic *IndexdClient) sortObject(ctx context.Context, target stores.TransferTarget, entry stores.ObjectMeta, key types.Hash256, runs []stores.SlabSlice, prefix string) ([]Find, uint64, error) {
	var (
		finds     []Find
		found     []transfer.File
		remainder []transfer.Part
		leftover  uint64
	)
	leave := func(at, length uint64) {
		remainder = append(remainder, transfer.Part{DataOffset: at, Length: length, Object: key})
		leftover += length
	}

	for _, run := range runs {
		var buf bytes.Buffer
		buf.Grow(int(run.Length))
		if err := ic.backend.Download(ctx, key, run.Offset, run.Length, &buf); err != nil {
			return nil, 0, fmt.Errorf("couldn't read %s: %w", entry.Path, err)
		}

		carved, _ := carve(buf.Bytes())
		var done uint64
		for _, find := range carved {
			if find.Offset > done {
				leave(run.Offset+done, find.Offset-done)
			}
			at := run.Offset + find.Offset
			finds = append(finds, Find{Offset: at, Length: find.Length, Ext: find.Ext})
			found = append(found, transfer.File{
				Path:       recoveredPath(prefix, key, at, find.Ext),
				Size:       find.Length,
				CreatedAt:  entry.CreatedAt,
				ModifiedAt: entry.ModifiedAt,
				Parts:      []transfer.Part{{DataOffset: at, Length: find.Length, Object: key}},
			})
			done = find.End()
		}
		if done < run.Length {
			leave(run.Offset+done, run.Length-done)
		}
	}

	if len(found) == 0 {
		return nil, leftover, nil
	}
	if err := ic.db.Recover(target, entry.Path, found, remainder); err != nil {
		return nil, 0, fmt.Errorf("couldn't make files of what was found in %s: %w", entry.Path, err)
	}

	// The object now holds files with names, which the indexer is told about so
	// that a rescue of this account comes to them rather than to lost+found again.
	ic.retag([]types.Hash256{key})

	return finds, leftover, nil
}

// objectRuns reports whether the file is made of one object of this account's and
// nothing else, which is what the objects of an account arrive as and all that is
// worth looking inside, and returns the object and the runs of it the file is made
// of. A file that was sorted before is made of what was left of it.
func objectRuns(db *stores.Database, acc stores.Account, share string, entry stores.ObjectMeta) (types.Hash256, []stores.SlabSlice, bool) {
	if entry.Size == 0 || entry.Size > maxCarveSize {
		return types.Hash256{}, nil, false
	}

	runs, err := db.GetMetadata(acc, share, entry.Path, 0, entry.Size)
	if err != nil || len(runs) == 0 {
		return types.Hash256{}, nil, false
	}

	key := runs[0].Key
	var covered uint64
	for _, run := range runs {
		if run.Key != key || key == (types.Hash256{}) || run.At != covered {
			return types.Hash256{}, nil, false
		}
		covered += run.Length
	}
	if covered != entry.Size {
		return types.Hash256{}, nil, false
	}

	return key, runs, true
}

// recoveredPath names a found file after where it was found, so that sorting the
// same objects again comes to the same files rather than to more of them.
func recoveredPath(prefix string, key types.Hash256, at uint64, ext string) string {
	return path.Join(prefix, RecoveredFolder, fmt.Sprintf("%s-%d.%s", key.String()[:8], at, ext))
}
