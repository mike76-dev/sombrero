package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"github.com/mike76-dev/sombrero/transfer"
	rhpv4 "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	sdk "go.sia.tech/siastorage"
)

// defaultCopyChunk is how much of a file is moved at a time where the caller
// names no size. A share's slab size is the better fit, since a piece that fills
// a slab is uploaded as one instead of waiting to be packed with others.
const defaultCopyChunk = rhpv4.SectorSize

// defaultCopyRetry is how long a copy waits before offering the destination more
// data after its staging area turned out to be full.
const defaultCopyRetry = 30 * time.Second

// PartReader fetches the bytes a part describes from wherever they are now. The
// offset is counted from the start of the part, not of the file.
type PartReader interface {
	ReadPart(ctx context.Context, part transfer.Part, offset, length uint64, w io.Writer) error
}

// RenterdSource reads the parts a renterd server holds, by the key it knows them
// by. Its slabs cannot be taken over as they are, so its bytes are copied.
type RenterdSource struct {
	Client Client
}

// ReadPart implements PartReader.
func (s RenterdSource) ReadPart(ctx context.Context, part transfer.Part, offset, length uint64, w io.Writer) error {
	if part.Source == nil {
		return errors.New("the part does not say where its bytes are")
	}

	return s.Client.Read(ctx, stores.Account{}, part.Source.Key, part.DataOffset+offset, length, w)
}

// ObjectReader reads the bytes of an object an indexd account holds.
type ObjectReader interface {
	Download(ctx context.Context, key types.Hash256, offset, length uint64, w io.Writer) error
}

// IndexdSource reads the parts an indexd account holds, by the key of the object
// that holds them.
type IndexdSource struct {
	Objects ObjectReader
}

// ReadPart implements PartReader.
func (s IndexdSource) ReadPart(ctx context.Context, part transfer.Part, offset, length uint64, w io.Writer) error {
	key := part.Object
	if key == (types.Hash256{}) {
		if part.Source == nil {
			return errors.New("the part does not say where its bytes are")
		}
		if err := key.UnmarshalText([]byte(part.Source.Key)); err != nil {
			return fmt.Errorf("the part names no object this account holds: %w", err)
		}
	}

	return s.Objects.Download(ctx, key, part.DataOffset+offset, length, w)
}

// IndexdAccount is a connection to an indexd account: what it holds, and the
// bytes of what it holds.
type IndexdAccount struct {
	*sdkBackend
}

// NewIndexdAccount wraps an SDK connection as the account behind it, which is
// what describing one and copying from one take.
func NewIndexdAccount(sdkClient *sdk.SDK) *IndexdAccount {
	return &IndexdAccount{sdkBackend: &sdkBackend{
		sdk:      sdkClient,
		objCache: make(map[types.Hash256]sdk.Object),
	}}
}

// CopyOptions says how a description is copied.
type CopyOptions struct {
	// Account is whose the copied folders and files are at the destination.
	Account stores.Account

	// ChunkSize is how much is moved at a time, and Retry how long to wait when
	// the destination has no room for more of it yet.
	ChunkSize uint64
	Retry     time.Duration

	// Directories makes the folder at the destination. The default makes it the
	// way the destination would make any other, which says nothing of the flags
	// the description carries.
	Directories func(ctx context.Context, dir transfer.Directory) error

	// Stamp is called for a file that has been copied, for a caller that can put
	// back the times the copy does not carry.
	Stamp func(ctx context.Context, file transfer.File) error

	// Progress is called as a file is copied, and OnError for a file that could
	// not be, which the copy counts and goes on from.
	Progress func(path string, copied, total uint64)
	OnError  func(path string, err error)
}

// CopyStats is what a copy came to.
type CopyStats struct {
	Directories int
	Files       int
	Skipped     int
	Failed      int
	Bytes       uint64

	// Waits counts the times the copy had to wait for room at the destination,
	// which is what a share that is filling faster than it drains looks like.
	Waits int
}

// Copy reads what a description names from the source and writes it to the
// destination, through the upload path the destination serves its clients with:
// the data is buffered, packed and uploaded the same as anything a client wrote,
// and the backpressure of the destination applies to a copy as much as to them.
//
// A file that cannot be copied is reported and counted, and the copy goes on to
// the next one. Only a cancelled copy or a description that cannot be read stops
// it.
func Copy(ctx context.Context, dst Client, src PartReader, r *transfer.Reader, opts CopyOptions) (stats CopyStats, err error) {
	if opts.ChunkSize == 0 {
		opts.ChunkSize = defaultCopyChunk
	}
	if opts.Retry == 0 {
		opts.Retry = defaultCopyRetry
	}
	if opts.Directories == nil {
		opts.Directories = func(ctx context.Context, dir transfer.Directory) error {
			err := dst.MakeDirectory(ctx, opts.Account, dir.Path)
			if errors.Is(err, stores.ErrDirectoryExists) {
				return nil
			}

			return err
		}
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
			if err := opts.Directories(ctx, *dir); err != nil {
				stats.Failed++
				report(opts, dir.Path, err)
				continue
			}
			stats.Directories++

		case file != nil:
			copied, err := copyFile(ctx, dst, src, *file, opts, &stats)
			switch {
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return stats, err
			case err != nil:
				stats.Failed++
				report(opts, file.Path, err)
			case copied:
				stats.Files++
			default:
				stats.Skipped++
			}
		}
	}
}

// copyFile moves one file, reporting whether it was copied or was there already.
func copyFile(ctx context.Context, dst Client, src PartReader, file transfer.File, opts CopyOptions, stats *CopyStats) (bool, error) {
	if _, err := dst.Object(ctx, opts.Account, file.Path); err == nil {
		// What is there already is left alone, which is how a copy that stopped
		// halfway is taken up again.
		return false, nil
	} else if !errors.Is(err, stores.ErrNotFound) {
		return false, fmt.Errorf("failed to look for the file at the destination: %w", err)
	}

	uploadID, err := dst.StartUpload(ctx, opts.Account, file.Path)
	if err != nil {
		return false, fmt.Errorf("failed to start the upload: %w", err)
	}

	if err := copyParts(ctx, dst, src, file, uploadID, opts, stats); err != nil {
		if aerr := dst.AbortUpload(ctx, file.Path, uploadID); aerr != nil {
			return false, fmt.Errorf("%w; the upload could not be called off either: %w", err, aerr)
		}

		return false, err
	}

	if err := dst.FinishUpload(ctx, file.Path, uploadID, nil); err != nil {
		return false, fmt.Errorf("failed to finish the upload: %w", err)
	}

	if opts.Stamp != nil {
		if err := opts.Stamp(ctx, file); err != nil {
			return true, fmt.Errorf("the file was copied, but its times were not: %w", err)
		}
	}

	return true, nil
}

// copyParts moves the runs of bytes a file is made of, a chunk at a time.
func copyParts(ctx context.Context, dst Client, src PartReader, file transfer.File, uploadID string, opts CopyOptions, stats *CopyStats) error {
	var copied uint64
	for _, part := range file.Parts {
		for at := uint64(0); at < part.Length; {
			if err := ctx.Err(); err != nil {
				return err
			}

			n := min(opts.ChunkSize, part.Length-at)

			var buf bytes.Buffer
			buf.Grow(int(n))
			if part.Inline != nil {
				buf.Write(part.Inline[at : at+n])
			} else if err := src.ReadPart(ctx, part, at, n, &buf); err != nil {
				return fmt.Errorf("failed to read the bytes at %d: %w", part.Offset+at, err)
			}

			if err := write(ctx, dst, file.Path, uploadID, part.Offset+at, n, buf.Bytes(), opts, stats); err != nil {
				return err
			}

			at += n
			copied += n
			stats.Bytes += n
			if opts.Progress != nil {
				opts.Progress(file.Path, copied, file.Size)
			}
		}
	}

	return nil
}

// chunkWriter is the part of a destination that a chunk is offered to.
type chunkWriter interface {
	Write(ctx context.Context, r io.Reader, path, uploadID string, partNumber int, offset, length uint64) (string, error)
}

// write offers one chunk to the destination, waiting where its staging area is
// full: a destination filling faster than it drains is to be waited for, not a
// reason to give up on the file.
func write(ctx context.Context, dst chunkWriter, path, uploadID string, offset, length uint64, data []byte, opts CopyOptions, stats *CopyStats) error {
	for {
		_, err := dst.Write(ctx, bytes.NewReader(data), path, uploadID, 0, offset, length)
		if !errors.Is(err, ErrBacklogFull) {
			return err
		}

		stats.Waits++
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.Retry):
		}
	}
}

// report hands a failure to the caller, for a copy that goes on without it.
func report(opts CopyOptions, path string, err error) {
	if opts.OnError != nil {
		opts.OnError(path, err)
	}
}
