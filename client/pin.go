package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"

	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	"go.sia.tech/core/types"
	"go.sia.tech/indexd/api/app"
	sdk "go.sia.tech/siastorage"
)

// TransferStore is the part of a store an import writes the rows of what it
// pinned to.
type TransferStore interface {
	ApplyDirectory(target stores.TransferTarget, dir transfer.Directory) (stores.ApplyResult, error)
	ApplyFile(target stores.TransferTarget, file transfer.File) (stores.ApplyResult, error)
}

// ObjectPinner takes over an object that is already on the network by pinning the
// sectors it is made of into this account. The bytes are not moved: the host
// keeps them and the indexer takes on paying for them.
//
// The account is the one the objects are to end up in, which is the destination
// of an import rather than the source it is reading: pinning into the account
// that already holds them would do nothing at all.
type ObjectPinner interface {
	PinObject(ctx context.Context, obj sdk.Object) error
}

// ObjectChecker says whether the account holds an object. A pinner that is one
// is asked to confirm that a pin took, since a request that was answered is not
// the same as an account that came to hold what it asked for.
type ObjectChecker interface {
	HasObject(ctx context.Context, key types.Hash256) (bool, error)
}

// PinObject takes over an object for this connection's account, which is what
// makes an IndexdClient the pinner of an import into its share.
func (ic *IndexdClient) PinObject(ctx context.Context, obj sdk.Object) error {
	return ic.backend.Pin(ctx, obj)
}

// HasObject reports whether this connection's account holds the object. An
// account that does not is told apart from an indexer that could not say.
func (ic *IndexdClient) HasObject(ctx context.Context, key types.Hash256) (bool, error) {
	_, err := ic.backend.Object(ctx, key)
	if err == nil {
		return true, nil
	}

	var httpErr *app.HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return false, nil
	}

	return false, err
}

// PinFile pins what every part of a file is made of and returns the file with
// those parts naming the objects this account holds, ready to be applied.
//
// A file is taken over whole or not at all. Where one part cannot be pinned, the
// ones before it stay pinned and unreferenced, which the orphan scan is there to
// find; what they cost until then is a slab apiece.
func PinFile(ctx context.Context, pinner ObjectPinner, file transfer.File) (transfer.File, error) {
	pinned := file
	pinned.Parts = make([]transfer.Part, len(file.Parts))

	for i, part := range file.Parts {
		if err := ctx.Err(); err != nil {
			return file, err
		}

		if part.Pin == nil {
			// The bytes are already this account's, or there are none to pin.
			pinned.Parts[i] = part
			continue
		}

		obj := sdk.NewUnsafeObject(part.Pin.DataKey, part.Pin.Slabs)
		if err := pinner.PinObject(ctx, obj); err != nil {
			return file, fmt.Errorf("failed to pin the object of the run at %d: %w", part.Offset, err)
		}

		part.Object = obj.ID()
		part.Pin = nil
		pinned.Parts[i] = part
	}

	return pinned, nil
}

// ImportOptions says how a description is taken over.
type ImportOptions struct {
	CopyOptions

	// Target is where the rows of what is pinned are written. What is copied
	// goes through the destination's upload path instead, which writes its own.
	Target stores.TransferTarget

	// Copy has everything copied, even what could have been pinned, for a source
	// whose data is to be left behind rather than taken over.
	Copy bool

	// Report is called with the running totals as each folder and file is done
	// with, for a caller that says how far an import has got.
	Report func(stats ImportStats)

	// OnRefusal is called for a file the indexer would not take over, which is
	// then copied instead. It is not a failure, and is kept apart from the
	// failures so that one of those is not lost among hundreds of these.
	OnRefusal func(path string, err error)
}

// ImportStats is what taking over a description came to.
type ImportStats struct {
	Directories int
	Pinned      int
	Copied      int
	Skipped     int
	Failed      int
	Unresolved  int
	Bytes       uint64
	Waits       int

	// Refused counts the files the indexer would not take over, which were
	// copied instead. A source on another indexer is every file of it.
	Refused int
}

// Import takes over what a description names: what can be pinned is pinned, and
// what cannot is copied. Pinning leaves the bytes where they are and has this
// account pay for them from now on; copying moves them and pays for them twice
// until the source lets go of its own.
//
// A file that can be had neither way is reported and counted, and the import goes
// on to the next one.
func Import(ctx context.Context, store TransferStore, dst Client, src PartReader, pinner ObjectPinner, r *transfer.Reader, opts ImportOptions) (stats ImportStats, err error) {
	copyOpts := opts.CopyOptions
	if copyOpts.ChunkSize == 0 {
		copyOpts.ChunkSize = defaultCopyChunk
	}
	if copyOpts.Retry == 0 {
		copyOpts.Retry = defaultCopyRetry
	}

	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		dir, file, err := r.Next()
		if errors.Is(err, io.EOF) {
			return stats, nil
		}
		if err != nil {
			return stats, err
		}

		switch {
		case dir != nil:
			// The folders are applied rather than made through the destination,
			// so that the flags the description carries come over with them.
			if _, err := store.ApplyDirectory(opts.Target, *dir); err != nil {
				stats.Failed++
				report(opts.CopyOptions, dir.Path, err)
			} else {
				stats.Directories++
			}

		case file != nil:
			if err := importFile(ctx, store, dst, src, pinner, *file, opts, copyOpts, &stats); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return stats, err
				}
				stats.Failed++
				report(opts.CopyOptions, file.Path, err)
			}
		}

		if opts.Report != nil {
			opts.Report(stats)
		}
	}
}

// importFile takes over one file, by pinning what it is made of where that is to
// be had and by copying it where it is not.
func importFile(ctx context.Context, store TransferStore, dst Client, src PartReader, pinner ObjectPinner, file transfer.File, opts ImportOptions, copyOpts CopyOptions, stats *ImportStats) error {
	if !opts.Copy && pinner != nil && pinnable(file) {
		pinned, err := PinFile(ctx, pinner, file)
		if err == nil {
			// A pin that was answered is not a pin that took: a source and a
			// destination on different networks, or on the same account, both
			// answer without this account coming to hold anything.
			took, cerr := pinTook(ctx, pinner, pinned)
			if cerr != nil {
				return cerr
			}
			if !took {
				err = errPinDidNotTake
			}
		}
		if err == nil {
			res, err := store.ApplyFile(opts.Target, pinned)
			if err != nil {
				return err
			}

			switch res {
			case stores.Applied:
				stats.Pinned++
			case stores.AlreadyThere:
				stats.Skipped++
			default:
				stats.Unresolved++
			}

			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		// What the indexer will not take over is still to be had the long way,
		// which is worth saying but is not a file that could not be imported.
		stats.Refused++
		if opts.OnRefusal != nil {
			opts.OnRefusal(file.Path, err)
		}
	}

	// A copy goes through the destination's upload path, which puts a file only
	// where there is a folder to put it in.
	if parent := path.Dir(file.Path); parent != "/" && parent != "." {
		if _, err := store.ApplyDirectory(opts.Target, transfer.Directory{Path: parent}); err != nil {
			return fmt.Errorf("failed to make the folder above it: %w", err)
		}
	}

	var copyStats CopyStats
	copied, err := copyFile(ctx, dst, src, file, copyOpts, &copyStats)
	stats.Bytes += copyStats.Bytes
	stats.Waits += copyStats.Waits
	if err != nil {
		return err
	}
	if copied {
		stats.Copied++
	} else {
		stats.Skipped++
	}

	return nil
}

// errPinDidNotTake is what a file is copied over after a pin the indexer
// answered without this account coming to hold anything.
var errPinDidNotTake = errors.New("the objects were not pinned into this share's account, so the file is copied instead")

// pinTook reports whether the account the objects were pinned into holds them.
// A pinner that cannot say is taken at its word.
//
// One object is asked after, not all of them: they were pinned into one account
// by one call apiece, so what became of the first is what became of the rest, and
// a file of thousands of runs is not worth thousands of questions.
func pinTook(ctx context.Context, pinner ObjectPinner, file transfer.File) (bool, error) {
	checker, ok := pinner.(ObjectChecker)
	if !ok {
		return true, nil
	}

	for _, part := range file.Parts {
		if part.Object == (types.Hash256{}) {
			continue
		}

		return checker.HasObject(ctx, part.Object)
	}

	return true, nil
}

// pinnable reports whether a file says what it takes to pin any of it.
func pinnable(file transfer.File) bool {
	for _, part := range file.Parts {
		if part.Pin != nil {
			return true
		}
	}

	return false
}
