package client

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/slabs"
	sdk "go.sia.tech/siastorage"
)

// LostAndFound is where the objects of an account are described, an account
// keeping objects rather than the names anything went by.
const LostAndFound = "/lost+found"

// AccountObjects is what describing an indexd account takes: the log of what it
// has pinned, and the objects the log names.
type AccountObjects interface {
	ListObjects(ctx context.Context, cursor slabs.Cursor, limit int) ([]PinnedObject, error)
	Object(ctx context.Context, key types.Hash256) (sdk.Object, error)
}

// probeSample is how many of an account's objects are looked at to see whether
// they say what they hold. The answer is the same for all of them in practice —
// a server either tags what it writes or does not — so this is enough to tell
// what an import of it would come to.
const probeSample = 20

// probePages is how far into the object log a look at an account reads. An
// account of more objects than that is reported as holding at least what was
// counted, since whoever asked is waiting and an exact count of a long log is
// one request per hundred of it.
const probePages = 5

// AccountProbe is what an account looks like before anything is taken over: how
// many objects it holds, how many of them were looked at, and how many of those
// say which files they are of. More says the log went on past where the look
// stopped, so the count is what was seen rather than all there is.
type AccountProbe struct {
	Objects int
	Looked  int
	Tagged  int
	More    bool
}

// ProbeAccount reports what an import of the account would find, so that nobody
// has to run one to learn that its objects cannot be named.
//
// Nothing is asked after twice: the log hands over the objects along with the
// events, so a page of it says both how much is there and whether any of it can
// name its files.
func ProbeAccount(ctx context.Context, src AccountObjects) (probe AccountProbe, err error) {
	held := make(map[types.Hash256]struct{})

	var cursor slabs.Cursor
	for page := 0; page < probePages; page++ {
		if err := ctx.Err(); err != nil {
			return probe, err
		}

		events, err := src.ListObjects(ctx, cursor, objectPageSize)
		if err != nil {
			return probe, fmt.Errorf("failed to list the objects of the account: %w", err)
		}
		if len(events) == 0 {
			return finishProbe(probe, held, false), nil
		}

		for _, ev := range events {
			if ev.Deleted {
				delete(held, ev.Key)
				continue
			}
			held[ev.Key] = struct{}{}

			if probe.Looked < probeSample && ev.Object != nil {
				probe.Looked++
				if _, ok := parseTag(ev.Object.Metadata()); ok {
					probe.Tagged++
				}
			}
		}

		last := events[len(events)-1]
		if !last.UpdatedAt.After(cursor.After) && last.Key == cursor.Key {
			return finishProbe(probe, held, false), nil
		}
		cursor = slabs.Cursor{After: last.UpdatedAt, Key: last.Key}

		if len(events) < objectPageSize {
			return finishProbe(probe, held, false), nil
		}
	}

	return finishProbe(probe, held, true), nil
}

// finishProbe counts what was seen and says whether there was more of it.
func finishProbe(probe AccountProbe, held map[types.Hash256]struct{}, more bool) AccountProbe {
	probe.Objects = len(held)
	probe.More = more

	return probe
}

// A Count is what an import of a source brings over: the files the source names,
// and the slabs it does not, each of which comes over as one lost+found entry.
type Count struct {
	Files int
	Slabs int
}

// Total is everything the import has to get through.
func (c Count) Total() int {
	return c.Files + c.Slabs
}

// CountAccount counts what an import of the account would bring over, which is
// not the same as the number of objects it holds: an object that says which files
// it is of may hold runs of several of them, and a large file is held in several
// objects. An object that says nothing counts as a slab.
//
// It costs the walk of the object log and nothing else, the log handing over the
// objects along with the events.
func CountAccount(ctx context.Context, src AccountObjects) (Count, error) {
	pinned, err := listPinned(ctx, src)
	if err != nil {
		return Count{}, err
	}

	files := make(map[string]struct{})
	var count Count
	for _, obj := range pinned {
		if obj.Object == nil {
			count.Slabs++
			continue
		}

		tag, ok := parseTag(obj.Object.Metadata())
		if !ok {
			count.Slabs++
			continue
		}
		for _, piece := range tag.Pieces {
			files[piece.Share+piece.Path] = struct{}{}
		}
	}
	count.Files = len(files)

	return count, nil
}

// DescribeAccount walks an indexd account's object log and describes what it
// holds.
//
// An object written by a server that tagged it says which runs of which files
// are in it, so those files are described as themselves, under the names they
// went by. An object that says nothing about itself can only be described as
// itself, under prefix, since an account keeps objects rather than paths. Each
// part carries what it takes to pin the object as well as where to fetch its
// bytes, so whoever applies the description can do either.
func DescribeAccount(ctx context.Context, src AccountObjects, w *transfer.Writer, origin, prefix string) (stats DescribeStats, err error) {
	if prefix == "" {
		prefix = LostAndFound
	}

	pinned, err := listPinned(ctx, src)
	if err != nil {
		return stats, err
	}

	// The log is walked by time, which says nothing about what an account holds
	// now, so the objects are read in an order that reads the same twice.
	keys := make([]types.Hash256, 0, len(pinned))
	for key := range pinned {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	files := make(map[string]*transfer.File)
	var shares []string
	var untagged []sdk.Object

	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		// The log hands over the objects it names, so one is only asked after
		// where the event came without it.
		obj := pinned[key].Object
		if obj == nil {
			fetched, err := src.Object(ctx, key)
			if err != nil {
				return stats, fmt.Errorf("failed to retrieve the object %s: %w", key, err)
			}
			obj = &fetched
		}

		tag, ok := parseTag(obj.Metadata())
		if !ok {
			untagged = append(untagged, *obj)
			continue
		}

		for _, piece := range tag.Pieces {
			if !slices.Contains(shares, piece.Share) {
				shares = append(shares, piece.Share)
			}
			collectPiece(files, piece, *obj, origin)
		}
	}

	if err := writeNamedFiles(w, files, shares, &stats); err != nil {
		return stats, err
	}

	// What said nothing about itself goes under the prefix, one file per object,
	// which is the most that can be said of it.
	if len(untagged) > 0 {
		if err := w.Directory(transfer.Directory{Path: prefix}); err != nil {
			return stats, err
		}
		stats.Directories++

		for _, obj := range untagged {
			if err := describeObject(w, obj, prefix, origin); err != nil {
				return stats, err
			}
			stats.Files++
		}
	}

	return stats, nil
}

// collectPiece adds one run of a file to what is known of that file so far.
func collectPiece(files map[string]*transfer.File, piece objectPiece, obj sdk.Object, origin string) {
	key := piece.Share + piece.Path
	file, ok := files[key]
	if !ok {
		file = &transfer.File{
			Path:       piece.Path,
			Size:       piece.Size,
			CreatedAt:  obj.CreatedAt(),
			ModifiedAt: obj.UpdatedAt(),
		}
		files[key] = file
	}

	// The size the piece carries was what the file measured then, and the runs
	// themselves say what it measures at least.
	if piece.Size > file.Size {
		file.Size = piece.Size
	}
	if end := piece.Offset + piece.Length; end > file.Size {
		file.Size = end
	}
	if obj.UpdatedAt().After(file.ModifiedAt) {
		file.ModifiedAt = obj.UpdatedAt()
	}

	// The object is named as the source's, not as one this server holds: the key
	// is the same either way, but a row may only name an object this account has
	// pinned, which is what applying the description after pinning it does.
	dataKey := obj.UnsafeDataKey()
	file.Parts = append(file.Parts, transfer.Part{
		Offset:     piece.Offset,
		DataOffset: piece.At,
		Length:     piece.Length,
		Pin:        &transfer.Pin{DataKey: dataKey, Slabs: obj.Slabs()},
		Source:     &transfer.Source{Kind: "indexd", Origin: origin, Key: obj.ID().String()},
	})
}

// writeNamedFiles writes the files the objects could name, their runs in order.
// A run that is missing is a hole rather than a reason to leave the file out: the
// object that held it is no longer the account's.
func writeNamedFiles(w *transfer.Writer, files map[string]*transfer.File, shares []string, stats *DescribeStats) error {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		file := files[key]
		sort.Slice(file.Parts, func(i, j int) bool { return file.Parts[i].Offset < file.Parts[j].Offset })

		// One account is one share's, unless its key was shared with another
		// workgroup. Where the objects name more than one share, the files are
		// kept apart by it rather than merged into one tree.
		if len(shares) > 1 {
			share := strings.TrimSuffix(key, file.Path)
			file.Path = path.Join("/", share, file.Path)
		}

		if err := w.File(*file); err != nil {
			return err
		}
		stats.Files++
	}

	return nil
}

// describeObject writes an object as a file of its own, which is what an object
// that says nothing about itself amounts to.
func describeObject(w *transfer.Writer, obj sdk.Object, prefix, origin string) error {
	size := obj.Size()
	file := transfer.File{
		Path:       path.Join(prefix, obj.ID().String()),
		Size:       size,
		CreatedAt:  obj.CreatedAt(),
		ModifiedAt: obj.UpdatedAt(),
	}

	// An object of no bytes is made of no parts, the same as an empty file.
	if size > 0 {
		dataKey := obj.UnsafeDataKey()
		file.Parts = []transfer.Part{{
			Offset: 0,
			Length: size,
			Pin:    &transfer.Pin{DataKey: dataKey, Slabs: obj.Slabs()},
			Source: &transfer.Source{Kind: "indexd", Origin: origin, Key: obj.ID().String()},
		}}
	}

	return w.File(file)
}

// listPinned folds an account's object log into what it holds now, which is the
// same walk the orphan scan makes of its own account.
func listPinned(ctx context.Context, src AccountObjects) (map[types.Hash256]PinnedObject, error) {
	pinned := make(map[types.Hash256]PinnedObject)

	var cursor slabs.Cursor
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		page, err := src.ListObjects(ctx, cursor, objectPageSize)
		if err != nil {
			return nil, fmt.Errorf("failed to list the objects of the account: %w", err)
		}
		if len(page) == 0 {
			return pinned, nil
		}

		for _, obj := range page {
			if obj.Deleted {
				delete(pinned, obj.Key)
				continue
			}
			pinned[obj.Key] = obj
		}

		// The log is read with a strict "greater than", so a page that ends where
		// the one before it did is the end of it rather than a reason to go round
		// again.
		last := page[len(page)-1]
		if !last.UpdatedAt.After(cursor.After) && last.Key == cursor.Key {
			return pinned, nil
		}
		cursor = slabs.Cursor{After: last.UpdatedAt, Key: last.Key}
	}
}

// Object retrieves an object of the account by its key, which is what describing
// what the account holds is made of.
func (b *sdkBackend) Object(ctx context.Context, key types.Hash256) (sdk.Object, error) {
	return b.object(ctx, key)
}
