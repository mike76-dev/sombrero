package client

import (
	"bytes"
	"context"
	"fmt"
	"path"

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

// SortReport is what one round of sorting out the lost and found came to: how
// many objects were looked inside, what was found in them, and how much belonged
// to no file anything could recognize.
//
// Last is the path of the file the round stopped at, which the next round is
// given to take up where this one left off, and More says there was one.
type SortReport struct {
	Objects   int
	Recovered int
	Skipped   int
	Bytes     uint64
	Leftover  uint64
	Last      string
	More      bool
}

// SortLostAndFound looks inside the files under the prefix for files that can be
// recognized by what they begin and end with, and makes each one a file of its
// own under the recovered folder.
//
// Nothing is uploaded and nothing is paid for twice: a found file is a run of the
// object that is already there, named after where it was found so that sorting
// the same objects again finds the same files and leaves them alone.
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
	var looked int
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if entry.IsDir || entry.Path <= after {
			continue
		}
		if looked >= limit {
			// There is more to look at, which the caller asks for in a round of
			// its own rather than waiting for all of it in this one.
			report.More = true
			break
		}

		looked++
		report.Last = entry.Path

		key, at, ok := wholeObject(ic.db, acc, ic.share, entry)
		if !ok {
			report.Skipped++
			continue
		}

		report.Objects++
		found, leftover, err := ic.sortObject(ctx, target, entry, key, at, prefix)
		if err != nil {
			return report, err
		}

		report.Leftover += leftover
		for _, find := range found {
			report.Recovered++
			report.Bytes += find.Length
		}
	}

	return report, nil
}

// sortObject looks inside one object and makes a file of everything it finds.
func (ic *IndexdClient) sortObject(ctx context.Context, target stores.TransferTarget, entry stores.ObjectMeta, key types.Hash256, at uint64, prefix string) ([]Find, uint64, error) {
	var buf bytes.Buffer
	buf.Grow(int(entry.Size))
	if err := ic.backend.Download(ctx, key, at, entry.Size, &buf); err != nil {
		return nil, 0, fmt.Errorf("couldn't read %s: %w", entry.Path, err)
	}

	finds, leftover := carve(buf.Bytes())
	for _, find := range finds {
		file := transfer.File{
			Path:       recoveredPath(prefix, key, at+find.Offset, find.Ext),
			Size:       find.Length,
			CreatedAt:  entry.CreatedAt,
			ModifiedAt: entry.ModifiedAt,
			Parts: []transfer.Part{{
				Offset:     0,
				DataOffset: at + find.Offset,
				Length:     find.Length,
				Object:     key,
			}},
		}

		if _, err := ic.db.ApplyFile(target, file); err != nil {
			return nil, 0, fmt.Errorf("couldn't make a file of what was found at %d in %s: %w", find.Offset, entry.Path, err)
		}
	}

	return finds, leftover, nil
}

// wholeObject reports whether the file is one whole object of this account's and
// nothing else, which is what the objects of an account arrive as and all that
// is worth looking inside. It returns the object and where in it the file sits.
func wholeObject(db *stores.Database, acc stores.Account, share string, entry stores.ObjectMeta) (types.Hash256, uint64, bool) {
	if entry.Size == 0 || entry.Size > maxCarveSize {
		return types.Hash256{}, 0, false
	}

	slices, err := db.GetMetadata(acc, share, entry.Path, 0, entry.Size)
	if err != nil || len(slices) != 1 {
		return types.Hash256{}, 0, false
	}

	slice := slices[0]
	if slice.Key == (types.Hash256{}) || slice.At != 0 || slice.Length != entry.Size {
		return types.Hash256{}, 0, false
	}

	return slice.Key, slice.Offset, true
}

// recoveredPath names a found file after where it was found, so that sorting the
// same objects again comes to the same files rather than to more of them.
func recoveredPath(prefix string, key types.Hash256, at uint64, ext string) string {
	return path.Join(prefix, RecoveredFolder, fmt.Sprintf("%s-%d.%s", key.String()[:8], at, ext))
}
