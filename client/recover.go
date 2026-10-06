package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"

	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
)

// CatalogSource is an account whose objects can be listed and read, which is
// what finding the catalogs it carries of itself takes.
type CatalogSource interface {
	AccountObjects
	ObjectReader
}

// FoundCatalog is the newest catalog an account carries of one connection: the
// share and the file it went by, and its bytes, sealed as the server wrote them.
type FoundCatalog struct {
	Share string
	Path  string
	Data  []byte
}

// ErrNoCatalog is returned for an account that carries no catalog of itself.
var ErrNoCatalog = errors.New("the account carries no catalog of itself")

// HeldObjects returns what says whether the account still holds an object, from
// one walk of its object log. A catalog can only point at data, and a restore
// asks this before pointing at what was unpinned since the catalog was written.
func HeldObjects(ctx context.Context, src AccountObjects) (func(types.Hash256) bool, error) {
	pinned, err := listPinned(ctx, src)
	if err != nil {
		return nil, err
	}

	return heldIn(pinned), nil
}

// heldIn is HeldObjects over a log that was walked already.
func heldIn(pinned map[types.Hash256]PinnedObject) func(types.Hash256) bool {
	return func(key types.Hash256) bool {
		_, ok := pinned[key]
		return ok
	}
}

// FindCatalogs looks through the tags of an account's objects for the catalogs
// the server wrote into its shares, and reads the newest whole one of every
// connection. An account may serve more than one: a workgroup on two shares of
// one indexer, or two workgroups on one key. Each keeps its catalogs in a folder
// of its own, which is what tells them apart here. With the catalogs comes what
// says which objects the account holds, from the same walk, for the restore to
// check them against.
//
// It costs the walk of the object log and the download of the catalogs, nothing
// else: the names come with the log.
func FindCatalogs(ctx context.Context, src CatalogSource) ([]FoundCatalog, func(types.Hash256) bool, error) {
	pinned, err := listPinned(ctx, src)
	if err != nil {
		return nil, nil, err
	}

	keys := make([]types.Hash256, 0, len(pinned))
	for key := range pinned {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	// Every catalog file the tags name, and the share each is of.
	files := make(map[string]*transfer.File)
	shares := make(map[string]string)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		obj := pinned[key].Object
		if obj == nil {
			fetched, err := src.Object(ctx, key)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to retrieve the object %s: %w", key, err)
			}
			obj = &fetched
		}

		tag, ok := parseTag(obj.Metadata())
		if !ok {
			continue
		}
		for _, piece := range tag.Pieces {
			if isCatalog(piece) {
				collectPiece(files, piece, *obj, "")
				shares[piece.Share+piece.Path] = piece.Share
			}
		}
	}

	// The catalogs of one connection are the files of one folder of one share.
	// The newest is the last by name, since the names are the times; one that
	// is not whole on the network is passed over for the one before it.
	folders := make(map[string][]string)
	for name, file := range files {
		folder := shares[name] + "\x00" + path.Dir(file.Path)
		folders[folder] = append(folders[folder], name)
	}
	order := make([]string, 0, len(folders))
	for folder := range folders {
		order = append(order, folder)
	}
	sort.Strings(order)

	reader := IndexdSource{Objects: src}
	var found []FoundCatalog
	for _, folder := range order {
		names := folders[folder]
		sort.Sort(sort.Reverse(sort.StringSlice(names)))

		for _, name := range names {
			file := files[name]
			sort.Slice(file.Parts, func(i, j int) bool { return file.Parts[i].Offset < file.Parts[j].Offset })
			if !file.Complete() {
				continue
			}

			var buf bytes.Buffer
			buf.Grow(int(file.Size))
			for _, part := range file.Parts {
				if err := reader.ReadPart(ctx, part, 0, part.Length, &buf); err != nil {
					return nil, nil, fmt.Errorf("failed to read the catalog %s: %w", file.Path, err)
				}
			}
			found = append(found, FoundCatalog{Share: shares[name], Path: file.Path, Data: buf.Bytes()})
			break
		}
	}

	if len(found) == 0 {
		return nil, nil, ErrNoCatalog
	}

	return found, heldIn(pinned), nil
}
