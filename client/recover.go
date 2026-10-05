package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
)

// CatalogSource is an account whose objects can be listed and read, which is
// what finding the catalog it carries of itself takes.
type CatalogSource interface {
	AccountObjects
	ObjectReader
}

// FoundCatalog is the newest catalog an account carries of itself: the file it
// went by, and its bytes, sealed as the server wrote them.
type FoundCatalog struct {
	Path string
	Data []byte
}

// ErrNoCatalog is returned for an account that carries no catalog of itself.
var ErrNoCatalog = errors.New("the account carries no catalog of itself")

// FindCatalog looks through the tags of an account's objects for the catalogs the
// server wrote into the share, and reads the newest one it can read in full. It
// costs the walk of the object log and the download of the catalog, nothing else:
// the names come with the log.
func FindCatalog(ctx context.Context, src CatalogSource) (FoundCatalog, error) {
	pinned, err := listPinned(ctx, src)
	if err != nil {
		return FoundCatalog{}, err
	}

	keys := make([]types.Hash256, 0, len(pinned))
	for key := range pinned {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	files := make(map[string]*transfer.File)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return FoundCatalog{}, err
		}
		obj := pinned[key].Object
		if obj == nil {
			fetched, err := src.Object(ctx, key)
			if err != nil {
				return FoundCatalog{}, fmt.Errorf("failed to retrieve the object %s: %w", key, err)
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
			}
		}
	}

	// The newest is the last by name, since the names are the times; one that
	// is not whole on the network is passed over for the one before it.
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	reader := IndexdSource{Objects: src}
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
				return FoundCatalog{}, fmt.Errorf("failed to read the catalog %s: %w", file.Path, err)
			}
		}

		return FoundCatalog{Path: file.Path, Data: buf.Bytes()}, nil
	}

	return FoundCatalog{}, ErrNoCatalog
}
