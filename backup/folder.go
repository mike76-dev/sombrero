package backup

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mike76-dev/sombrero/transfer"
)

// Stored is a catalog found in the folder on this machine, by what it says of
// itself: a catalog of a connection names its share and workgroup, a catalog of
// the server neither.
type Stored struct {
	Path      string
	Kind      string
	Share     string
	Workgroup uuid.UUID
	WrittenAt time.Time
	Size      int64
}

// ListFolder returns the catalogs in the folder and the folders under it, newest
// first. A file that does not read as a catalog is left out: the folder is the
// server's, but nothing stops somebody from putting something else there.
func ListFolder(dir string) ([]Stored, error) {
	if dir == "" {
		return nil, nil
	}

	var stored []Stored
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".catalog") {
			return nil
		}
		if s, ok := describe(path); ok {
			stored = append(stored, s)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(stored, func(i, j int) bool { return stored[i].WrittenAt.After(stored[j].WrittenAt) })

	return stored, nil
}

// describe reads what a catalog file says of itself, which takes its header and
// the record after it and nothing more.
func describe(path string) (Stored, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Stored{}, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return Stored{}, false
	}
	r, err := transfer.NewReader(f)
	if err != nil {
		return Stored{}, false
	}

	s := Stored{Path: path, WrittenAt: r.Header().CreatedAt, Size: info.Size()}
	switch {
	case r.Connection() != nil:
		s.Kind = "connection"
		s.Share = r.Connection().Share.Name
		s.Workgroup = uuid.UUID(r.Connection().Workgroup.UUID)
	case r.Server() != nil:
		s.Kind = "server"
	default:
		return Stored{}, false
	}

	return s, true
}
