package client

import (
	"context"
	"fmt"
	"path"
	"sort"

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

// DescribeAccount walks an indexd account's object log and describes what it
// holds, each object at a path of its own under prefix.
//
// The names the objects went by are not the account's to know — it keeps objects,
// not paths — so this describes data that was stranded, or that is being taken
// over from a server that cannot say what it called it. Each part carries what it
// takes to pin the object as well as where to fetch its bytes, so whoever applies
// the description can do either.
func DescribeAccount(ctx context.Context, src AccountObjects, w *transfer.Writer, origin, prefix string) (stats DescribeStats, err error) {
	if prefix == "" {
		prefix = LostAndFound
	}

	pinned, err := listPinned(ctx, src)
	if err != nil {
		return stats, err
	}

	if err := w.Directory(transfer.Directory{Path: prefix}); err != nil {
		return stats, err
	}
	stats.Directories++

	// The log is walked by time, which says nothing about what an account holds
	// now, so the objects are described in an order that reads the same twice.
	keys := make([]types.Hash256, 0, len(pinned))
	for key := range pinned {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		obj, err := src.Object(ctx, key)
		if err != nil {
			return stats, fmt.Errorf("failed to retrieve the object %s: %w", key, err)
		}

		size := obj.Size()
		file := transfer.File{
			Path:       path.Join(prefix, key.String()),
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
				Source: &transfer.Source{Kind: "indexd", Origin: origin, Key: key.String()},
			}}
		}

		if err := w.File(file); err != nil {
			return stats, err
		}
		stats.Files++
	}

	return stats, nil
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
