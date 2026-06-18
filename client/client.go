package client

import (
	"context"
	"io"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/renterd/v2/api"
)

// GeneralInfo contains some general information about the share.
type GeneralInfo struct {
	Bucket    string // a renterd only thing
	CreatedAt time.Time
}

// StorageInfo contains all needed information about the underlying storage.
type StorageInfo struct {
	Type             string
	RemainingStorage uint64
	UsedStorage      uint64
	MinShards        int
	TotalShards      int
}

// FileInfo is a helper structure that combines the 64-bit and the 128-bit file IDs and the file creation time.
type FileInfo struct {
	ID64       uint64
	ID         []byte
	CreatedAt  time.Time
	ModifiedAt time.Time
}

// ObjectInfo contains the most important information about an object.
type ObjectInfo struct {
	Key        string
	ETag       string
	CreatedAt  time.Time
	ModifiedAt time.Time
	Size       uint64
}

// Client provides an interface for accessing Sia-based remote shares.
type Client interface {
	Info(ctx context.Context) (GeneralInfo, error)
	Storage(ctx context.Context) (StorageInfo, error)
	IsEmpty(ctx context.Context, acc stores.Account, path string) (bool, error)
	List(ctx context.Context, acc stores.Account, path string) ([]ObjectInfo, error)
	Object(ctx context.Context, acc stores.Account, path string) (ObjectInfo, error)
	Parents(ctx context.Context, acc stores.Account, path string) (currentDir, parentDir FileInfo, err error)
	Read(ctx context.Context, acc stores.Account, path string, offset, length uint64, buf io.Writer) error
	StartUpload(ctx context.Context, acc stores.Account, path string) (uploadID string, err error)
	AbortUpload(ctx context.Context, path string, uploadID string) (err error)
	FinishUpload(ctx context.Context, path string, uploadID string, parts []api.MultipartCompletedPart) error
	Write(ctx context.Context, r io.Reader, path string, uploadID string, partNumber int, offset, length uint64) (eTag string, err error)
	Delete(ctx context.Context, acc stores.Account, path string, batch bool) error
	Rename(ctx context.Context, acc stores.Account, oldName, newName string, isDir, force bool) error
	MakeDirectory(ctx context.Context, acc stores.Account, path string) error
	DeleteAll(ctx context.Context) error
	Close() error
}

// sizeFromSeeker tries to find out the size of a file.
func sizeFromSeeker(r io.Reader) (int64, error) {
	s, ok := r.(io.Seeker)
	if !ok {
		return 0, nil
	}
	size, err := s.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	_, err = s.Seek(0, io.SeekStart)
	if err != nil {
		return 0, err
	}
	return size, nil
}
