package client

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/core/types"
	"go.sia.tech/renterd/v2/api"
)

// ErrNoSlabScan is returned by the backends that do not pin slabs of their own.
// A renterd share keeps its objects in renterd's own database and deletes them
// with the files, so there is nothing for this server to reconcile.
var ErrNoSlabScan = errors.New("slab scanning is only supported by indexd shares")

// DefaultOrphanMinAge is how long a slab that nothing references is given
// before it counts as orphaned. An upload pins its slab before it records it,
// so a slab that has just appeared is far more likely to belong to an upload
// in flight than to a deleted file, and unpinning it would destroy the data of
// a file that is about to reference it.
const DefaultOrphanMinAge = time.Hour

// OrphanedSlab is a slab that a share's connection has pinned, and pays for,
// while no file in the database references it.
type OrphanedSlab struct {
	Key      types.Hash256 `json:"key"`
	Size     uint64        `json:"size"`
	PinnedAt time.Time     `json:"pinnedAt"`
}

// FragmentationReport is what a connection reports about the dead space in its
// slabs: the totals over all of them, and the slabs that reach Threshold, most
// fragmented first.
type FragmentationReport struct {
	Threshold float64                   `json:"threshold"`
	Stats     stores.FragmentationStats `json:"stats"`
	Slabs     []stores.PackedSlab       `json:"slabs"`
}

// DefragmentReport is what one round of defragmenting achieved: the slabs it
// emptied, the bytes it moved back into the upload queue to be packed again,
// and the dead space those slabs held.
type DefragmentReport struct {
	Slabs     int    `json:"slabs"`
	Moved     uint64 `json:"moved"`
	Reclaimed uint64 `json:"reclaimed"`
}

// UnpinResult reports what unpinning a share's orphaned slabs achieved.
type UnpinResult struct {
	// Unpinned counts the slabs that were dropped from the backend, and Freed
	// is what they occupied.
	Unpinned int    `json:"unpinned"`
	Freed    uint64 `json:"freed"`

	// Failed counts the slabs the backend would not drop. They stay staged in
	// the database and are retried in the background, so they are not lost.
	Failed int `json:"failed"`
}

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

	// Parents describes the directory at path and the one above it, which is what a listing
	// carries as its "." and ".." entries. The share root stands in for either of them where
	// there is none.
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

	// OrphanedSlabs reports the slabs this connection has pinned that no file
	// references and that have been pinned for at least minAge, and
	// UnpinOrphanedSlabs drops exactly what the same scan finds when it runs.
	// Both return ErrNoSlabScan on the backends that manage their own objects.
	OrphanedSlabs(ctx context.Context, minAge time.Duration) ([]OrphanedSlab, error)
	UnpinOrphanedSlabs(ctx context.Context, minAge time.Duration) (UnpinResult, error)

	// Fragmentation reports the dead space in this connection's slabs, listing
	// those that are dead space by at least threshold. A threshold outside of
	// (0, 1] reports at the level the connection is configured with. It
	// returns ErrNoSlabScan on the backends that manage their own objects.
	Fragmentation(ctx context.Context, threshold float64) (FragmentationReport, error)

	// Defragment moves what is left in the fragmented slabs back into the
	// upload queue, to be packed into fewer slabs. One call does at most one
	// round, which is as many slabs as it takes to leave the share paying for
	// one less; a round that reports no slabs found nothing worth moving. It
	// returns ErrNoSlabScan on the backends that manage their own objects.
	Defragment(ctx context.Context) (DefragmentReport, error)

	Close() error
}

// SecurityDescriber is the part of a client that knows who owns a path and who
// holds grants on it, for the real security descriptors. The renterd backend
// has no owners, so it does not implement it and keeps the synthetic answer.
type SecurityDescriber interface {
	PathSecurity(ctx context.Context, path string) (stores.PathSecurity, error)
}

// AccessChecker is the part of a client whose store can say what an account may
// do at one path, for the server to narrow a handle's access rights to.
type AccessChecker interface {
	PathAccess(ctx context.Context, acc stores.Account, path string) (stores.PathAccess, error)
}

// VersionInfo describes a previous version of a file: when its contents were
// last written, and when and by whom they were replaced.
type VersionInfo struct {
	ID         uint64
	Size       uint64
	ModifiedAt time.Time
	ReplacedAt time.Time
	ReplacedBy string
}

// Versioner is a client whose shares keep the previous versions of files.
type Versioner interface {
	// Versions lists the versions of the file the account may read, newest first.
	Versions(ctx context.Context, acc stores.Account, path string) ([]VersionInfo, error)

	// ReadVersion reads a range of the version with the ID into w.
	ReadVersion(ctx context.Context, acc stores.Account, path string, id uint64, offset, length uint64, w io.Writer) error

	// RestoreVersion makes the version the contents of the file again, keeping
	// the contents it replaces as the policy says.
	RestoreVersion(ctx context.Context, acc stores.Account, path string, id uint64) error

	// DeleteVersions removes the version with the ID, or every version of the
	// file with a zero ID.
	DeleteVersions(ctx context.Context, acc stores.Account, path string, id uint64) error

	// EmptyFile removes the contents of the file ahead of their replacement,
	// keeping them as the policy says, with the versions of the file in place.
	EmptyFile(ctx context.Context, acc stores.Account, path string) error
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
