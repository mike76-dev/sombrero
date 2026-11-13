package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/renterd/v2/api"
)

// renterdReservedFolder is the server's own folder of a renterd share, which
// no client sees or touches; renterdVersionsFolder in it is where the share
// keeps the contents of its versions, each as an object named after the ID of
// its record.
const (
	renterdReservedFolder = ".sombrero"
	renterdVersionsFolder = renterdReservedFolder + "/versions"
)

// RenterdVersions is a renterd client whose share keeps the previous versions
// of its files: their contents as hidden objects of the bucket, their records
// in the database. The callers answer for the rights of the account.
type RenterdVersions struct {
	*RenterdClient
	db    *stores.Database
	share string

	// The account writing each upload under way, which FinishUpload is not told.
	mu        sync.Mutex
	uploaders map[string]stores.Account
}

// WithRenterdVersions makes the renterd client keep versions, recorded in the
// database under the share. Any other client is returned as it is.
func WithRenterdVersions(c Client, db *stores.Database, share string) Client {
	rc, ok := c.(*RenterdClient)
	if !ok {
		return c
	}
	return &RenterdVersions{RenterdClient: rc, db: db, share: share, uploaders: make(map[string]stores.Account)}
}

// versionKey is the object holding the contents of the version with the ID.
func versionKey(id uint64) string {
	return fmt.Sprintf("%s/%d", renterdVersionsFolder, id)
}

// reserved says whether the path is the server's folder or anything in it.
func reserved(path string) bool {
	p := strings.Trim(strings.ReplaceAll(path, "\\", "/"), "/")
	return p == renterdReservedFolder || strings.HasPrefix(p, renterdReservedFolder+"/")
}

// IsEmpty implements Client: the server's folder is not there to a client.
func (rv *RenterdVersions) IsEmpty(ctx context.Context, acc stores.Account, path string) (bool, error) {
	if reserved(path) {
		return false, stores.ErrNotFound
	}
	return rv.RenterdClient.IsEmpty(ctx, acc, path)
}

// List implements Client, leaving the server's folder out.
func (rv *RenterdVersions) List(ctx context.Context, acc stores.Account, path string) ([]ObjectInfo, error) {
	if reserved(path) {
		return nil, stores.ErrNotFound
	}
	ois, err := rv.RenterdClient.List(ctx, acc, path)
	if err != nil {
		return nil, err
	}
	listed := make([]ObjectInfo, 0, len(ois))
	for _, oi := range ois {
		if !reserved(oi.Key) {
			listed = append(listed, oi)
		}
	}
	return listed, nil
}

// Object implements Client: the server's folder is not there to a client.
func (rv *RenterdVersions) Object(ctx context.Context, acc stores.Account, path string) (ObjectInfo, error) {
	if reserved(path) {
		return ObjectInfo{}, stores.ErrNotFound
	}
	return rv.RenterdClient.Object(ctx, acc, path)
}

// Parents implements Client: the server's folder is not there to a client.
func (rv *RenterdVersions) Parents(ctx context.Context, acc stores.Account, path string) (FileInfo, FileInfo, error) {
	if reserved(path) {
		return FileInfo{}, FileInfo{}, stores.ErrNotFound
	}
	return rv.RenterdClient.Parents(ctx, acc, path)
}

// Read implements Client: the server's folder is not there to a client.
func (rv *RenterdVersions) Read(ctx context.Context, acc stores.Account, path string, offset, length uint64, buf io.Writer) error {
	if reserved(path) {
		return stores.ErrNotFound
	}
	return rv.RenterdClient.Read(ctx, acc, path, offset, length, buf)
}

// MakeDirectory implements Client: the server's folder is there already.
func (rv *RenterdVersions) MakeDirectory(ctx context.Context, acc stores.Account, path string) error {
	if reserved(path) {
		return stores.ErrDirectoryExists
	}
	return rv.RenterdClient.MakeDirectory(ctx, acc, path)
}

// file is the object at the path where there is a file: a folder, the server's
// folder or nothing there is reported as ErrNotFound.
func (rv *RenterdVersions) file(ctx context.Context, acc stores.Account, path string) (ObjectInfo, error) {
	if reserved(path) {
		return ObjectInfo{}, stores.ErrNotFound
	}
	oi, err := rv.RenterdClient.Object(ctx, acc, path)
	if errors.Is(err, stores.ErrNotFound) || err == nil && strings.HasSuffix(oi.Key, "/") {
		return ObjectInfo{}, stores.ErrNotFound
	}
	return oi, err
}

// stash moves the contents of the file out of the way as a version, where the
// policy keeps them, and says whether it did; otherwise they are left for the
// caller to overwrite or delete.
func (rv *RenterdVersions) stash(ctx context.Context, acc stores.Account, path string, live ObjectInfo, deliberate bool) (bool, error) {
	id, pruned, err := rv.db.KeepRenterdFile(stores.RenterdReplacement{
		Share: rv.share, Path: path, Account: acc, Size: live.Size, ModifiedAt: live.ModifiedAt, Deliberate: deliberate,
	})
	if err != nil {
		return false, err
	}
	rv.dropObjects(ctx, pruned)
	if id == 0 {
		return false, nil
	}
	return true, rv.moveAside(ctx, acc, path, id)
}

// moveAside renames the file to the object of the version with the ID, or
// forgets the version where it cannot.
func (rv *RenterdVersions) moveAside(ctx context.Context, acc stores.Account, path string, id uint64) error {
	if err := rv.RenterdClient.Rename(ctx, acc, path, versionKey(id), false, true); err != nil {
		if ferr := rv.db.ForgetRenterdVersion(id); ferr != nil {
			log.Printf("failed to forget version %d of %s: %v", id, path, ferr)
		}
		return fmt.Errorf("failed to keep the contents of %s: %w", path, err)
	}
	return nil
}

// dropObjects deletes the objects of the versions whose records are gone. One
// that is gone already is fine; one that will not go is left behind.
func (rv *RenterdVersions) dropObjects(ctx context.Context, ids []uint64) {
	for _, id := range ids {
		if err := rv.RenterdClient.Delete(ctx, stores.Account{}, versionKey(id), false); err != nil && !errors.Is(err, stores.ErrNotFound) {
			log.Printf("failed to delete the object of version %d: %v", id, err)
		}
	}
}

// StartUpload implements Client, remembering who is writing. Nothing is
// written into the server's folder.
func (rv *RenterdVersions) StartUpload(ctx context.Context, acc stores.Account, path string) (string, error) {
	if reserved(path) {
		return "", stores.ErrAccessDenied
	}
	id, err := rv.RenterdClient.StartUpload(ctx, acc, path)
	if err == nil && id != "" {
		rv.mu.Lock()
		rv.uploaders[id] = acc
		rv.mu.Unlock()
	}
	return id, err
}

// uploader is the account that started the upload, forgotten from here on.
func (rv *RenterdVersions) uploader(id string) stores.Account {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	acc := rv.uploaders[id]
	delete(rv.uploaders, id)
	return acc
}

// AbortUpload implements Client.
func (rv *RenterdVersions) AbortUpload(ctx context.Context, path string, uploadID string) error {
	rv.uploader(uploadID)
	return rv.RenterdClient.AbortUpload(ctx, path, uploadID)
}

// FinishUpload implements Client. The file the upload replaces is kept as a
// version where the policy says, before the upload takes its name.
func (rv *RenterdVersions) FinishUpload(ctx context.Context, path string, uploadID string, parts []api.MultipartCompletedPart) error {
	acc := rv.uploader(uploadID)
	if strings.HasSuffix(path, ":Zone.Identifier") {
		return nil
	}
	live, err := rv.file(ctx, acc, path)
	if err == nil {
		if _, err := rv.stash(ctx, acc, path, live, false); err != nil {
			return err
		}
	} else if !errors.Is(err, stores.ErrNotFound) {
		return err
	}
	return rv.RenterdClient.FinishUpload(ctx, path, uploadID, parts)
}

// Delete implements Client. A file goes the way EmptyFile takes it, so that it
// lives on as a version, or as the previous contents of the file that took its
// name, where it was renamed away from that name moments ago. A folder takes
// the versions of its files with it.
func (rv *RenterdVersions) Delete(ctx context.Context, acc stores.Account, path string, batch bool) error {
	if reserved(path) {
		return stores.ErrNotFound
	}
	// A folder goes with what is in it; the versions under it stay, found
	// by their paths, so that the files can be restored.
	if batch {
		return rv.RenterdClient.Delete(ctx, acc, path, true)
	}

	live, err := rv.file(ctx, acc, path)
	if err != nil {
		return err
	}
	origin, err := rv.db.RenterdDisplacedOrigin(rv.share, path)
	if err != nil {
		return err
	}
	if origin != "" {
		if _, err := rv.file(ctx, acc, strings.TrimPrefix(origin, "/")); err == nil {
			return rv.absorb(ctx, acc, path, live, origin)
		} else if !errors.Is(err, stores.ErrNotFound) {
			return err
		}
	}

	kept, err := rv.stash(ctx, acc, path, live, true)
	if err != nil || kept {
		return err
	}
	return rv.RenterdClient.Delete(ctx, acc, path, false)
}

// absorb makes the file the previous contents of the one at the origin, which
// it was renamed away from: kept as a version of it where the session rule
// says, deleted as a save's leftovers otherwise.
func (rv *RenterdVersions) absorb(ctx context.Context, acc stores.Account, path string, live ObjectInfo, origin string) error {
	id, pruned, err := rv.db.AbsorbRenterdFile(stores.RenterdReplacement{
		Share: rv.share, Path: path, Account: acc, Size: live.Size, ModifiedAt: live.ModifiedAt,
	}, origin)
	if err != nil {
		return err
	}
	rv.dropObjects(ctx, pruned)
	if id == 0 {
		return rv.RenterdClient.Delete(ctx, acc, path, false)
	}
	return rv.moveAside(ctx, acc, path, id)
}

// Rename implements Client. The versions follow the file, or the files of the
// folder, and a file renamed over is kept as a version where the policy says.
// Nothing is renamed into the server's folder.
func (rv *RenterdVersions) Rename(ctx context.Context, acc stores.Account, oldName, newName string, isDir, force bool) error {
	if reserved(oldName) {
		return stores.ErrNotFound
	}
	if reserved(newName) {
		return stores.ErrAccessDenied
	}
	if !isDir && force {
		over, err := rv.file(ctx, acc, newName)
		if err == nil {
			if _, err := rv.stash(ctx, acc, newName, over, false); err != nil {
				return err
			}
		} else if !errors.Is(err, stores.ErrNotFound) {
			return err
		}
	}
	if err := rv.RenterdClient.Rename(ctx, acc, oldName, newName, isDir, force); err != nil {
		return err
	}
	return rv.db.RenterdFileRenamed(rv.share, oldName, newName, isDir)
}

// Versions implements Versioner.
func (rv *RenterdVersions) Versions(ctx context.Context, acc stores.Account, path string) ([]VersionInfo, error) {
	if reserved(path) {
		return nil, stores.ErrNotFound
	}
	versions, err := rv.db.RenterdVersions(rv.share, path)
	if err != nil {
		return nil, err
	}
	infos := make([]VersionInfo, 0, len(versions))
	for _, v := range versions {
		infos = append(infos, versionInfo(v))
	}
	return infos, nil
}

// versionInfo is the version as the client reports it.
func versionInfo(v stores.Version) VersionInfo {
	return VersionInfo{ID: v.ID, Size: v.Size, ModifiedAt: v.ModifiedAt, ReplacedAt: v.ReplacedAt, ReplacedBy: v.ReplacedBy, Kept: v.Kept}
}

// ReadVersion implements Versioner.
func (rv *RenterdVersions) ReadVersion(ctx context.Context, acc stores.Account, path string, id uint64, offset, length uint64, buf io.Writer) error {
	if _, err := rv.db.RenterdVersion(rv.share, path, id); err != nil {
		return err
	}
	return rv.RenterdClient.Read(ctx, acc, versionKey(id), offset, length, buf)
}

// RestoreVersion implements Versioner. The contents the version replaces are
// kept as a version, unless they are a restored version untouched, and the
// version stays, copied onto the file.
func (rv *RenterdVersions) RestoreVersion(ctx context.Context, acc stores.Account, path string, id uint64) error {
	v, err := rv.db.RenterdVersion(rv.share, path, id)
	if err != nil {
		return err
	}
	live, err := rv.file(ctx, acc, path)
	if err == nil {
		if _, err := rv.stash(ctx, acc, path, live, true); err != nil {
			return err
		}
	} else if !errors.Is(err, stores.ErrNotFound) {
		return err
	}

	err = rv.doRequest(ctx, "POST", "/api/bus/objects/copy", api.CopyObjectsRequest{
		SourceBucket:      rv.bucket,
		SourceKey:         "/" + versionKey(id),
		DestinationBucket: rv.bucket,
		DestinationKey:    "/" + strings.TrimPrefix(path, "/"),
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to copy the version onto the file: %w", err)
	}
	return rv.db.MarkRenterdRestored(rv.share, path, v.Size, []uint64{id})
}

// DeleteVersions implements Versioner.
func (rv *RenterdVersions) DeleteVersions(ctx context.Context, acc stores.Account, path string, id uint64) error {
	ids, err := rv.db.DeleteRenterdVersions(rv.share, path, id)
	if err != nil {
		return err
	}
	rv.dropObjects(ctx, ids)
	return nil
}

// DeleteVersionsUnder implements Versioner.
func (rv *RenterdVersions) DeleteVersionsUnder(ctx context.Context, acc stores.Account, dir string) error {
	ids, err := rv.db.DeleteRenterdVersionsUnder(rv.share, dir)
	if err != nil {
		return err
	}
	rv.dropObjects(ctx, ids)
	return nil
}

// KeepVersion implements Versioner.
func (rv *RenterdVersions) KeepVersion(ctx context.Context, acc stores.Account, path string, id uint64, keep bool) error {
	return rv.db.KeepRenterdVersion(rv.share, path, id, keep)
}

// EmptyFile implements Versioner.
func (rv *RenterdVersions) EmptyFile(ctx context.Context, acc stores.Account, path string, deliberate bool) error {
	live, err := rv.file(ctx, acc, path)
	if err != nil {
		return err
	}
	kept, err := rv.stash(ctx, acc, path, live, deliberate)
	if err != nil || kept {
		return err
	}
	return rv.RenterdClient.Delete(ctx, acc, path, false)
}

// MarkRestored implements Versioner.
func (rv *RenterdVersions) MarkRestored(ctx context.Context, path string, ids []uint64) error {
	live, err := rv.file(ctx, stores.Account{}, path)
	if err != nil {
		return err
	}
	return rv.db.MarkRenterdRestored(rv.share, path, live.Size, ids)
}

// Snapshots implements Versioner.
func (rv *RenterdVersions) Snapshots(ctx context.Context, acc stores.Account, path string) ([]time.Time, error) {
	if reserved(path) {
		return nil, stores.ErrNotFound
	}
	return rv.db.RenterdSnapshots(rv.share, path)
}

// ExpireVersions drops the versions of the share replaced longer ago than the
// age, the kept ones excepted, and returns how many went.
func (rv *RenterdVersions) ExpireVersions(ctx context.Context, age time.Duration) (int, error) {
	ids, err := rv.db.ExpireRenterdVersions(rv.share, age)
	if err != nil {
		return 0, err
	}
	rv.dropObjects(ctx, ids)
	return len(ids), nil
}

// copyAt is the copy of a file that stood at the time among its versions and
// the file as it is, which has a zero ID and no replacement time: one made by
// then and not replaced by then, with the contents of the time where they were
// kept, and the nearest kept after it otherwise. Times go by the second, as the
// snapshot names do; a file not made by then has no copy.
func copyAt(copies []VersionInfo, at time.Time) (VersionInfo, bool) {
	made := func(v VersionInfo) bool { return !v.ModifiedAt.Truncate(time.Second).After(at) }
	existed := false
	var current []VersionInfo
	for _, v := range copies {
		existed = existed || made(v)
		if v.ReplacedAt.IsZero() || v.ReplacedAt.After(at) {
			current = append(current, v)
		}
	}
	if !existed || len(current) == 0 {
		return VersionInfo{}, false
	}
	sort.SliceStable(current, func(i, j int) bool {
		a, b := current[i], current[j]
		if made(a) != made(b) {
			return made(a)
		}
		if a.ReplacedAt.IsZero() != b.ReplacedAt.IsZero() {
			return !a.ReplacedAt.IsZero()
		}
		return a.ReplacedAt.Before(b.ReplacedAt)
	})
	return current[0], true
}

// ObjectAt implements Versioner.
func (rv *RenterdVersions) ObjectAt(ctx context.Context, acc stores.Account, path string, at time.Time) (VersionInfo, error) {
	if reserved(path) {
		return VersionInfo{}, stores.ErrNotFound
	}
	copies, err := rv.Versions(ctx, acc, path)
	if err != nil {
		return VersionInfo{}, err
	}
	live, err := rv.file(ctx, acc, path)
	if err == nil {
		copies = append(copies, VersionInfo{Size: live.Size, ModifiedAt: live.ModifiedAt})
	} else if !errors.Is(err, stores.ErrNotFound) {
		return VersionInfo{}, err
	}
	v, ok := copyAt(copies, at)
	if !ok {
		return VersionInfo{}, stores.ErrNotFound
	}
	return v, nil
}

// ListAt implements Versioner. The folder as it was holds the files, kept or
// current, that stood at the time, and the subfolders any of those stood in,
// which a listing of the whole subtree tells, renterd dating no folder: one
// nothing was made in by then was not there.
func (rv *RenterdVersions) ListAt(ctx context.Context, acc stores.Account, dir string, at time.Time) ([]ObjectInfo, error) {
	if reserved(dir) {
		return nil, stores.ErrNotFound
	}
	prefix := "/" + strings.Trim(dir, "/")
	if prefix != "/" {
		prefix += "/"
	}
	live, err := rv.listAll(ctx, strings.TrimSuffix(dir, "/")+"/")
	if err != nil {
		return nil, err
	}
	versions, err := rv.db.RenterdVersionsUnder(rv.share, dir)
	if err != nil {
		return nil, err
	}

	// The copies of every path under the folder, at any depth; a subfolder is
	// there while anything in it has a copy current at the time.
	copies := make(map[string][]VersionInfo)
	for _, oi := range live {
		if oi.Key != prefix && !reserved(oi.Key) {
			copies[oi.Key] = append(copies[oi.Key], VersionInfo{Size: oi.Size, ModifiedAt: oi.ModifiedAt})
		}
	}
	for _, v := range versions {
		copies[v.Path] = append(copies[v.Path], versionInfo(v.Version))
	}

	var ois []ObjectInfo
	dirs := make(map[string]bool)
	existed := false
	for key, cs := range copies {
		for _, c := range cs {
			existed = existed || !c.ModifiedAt.Truncate(time.Second).After(at)
		}
		v, ok := copyAt(cs, at)
		if !ok {
			continue
		}
		rel := strings.TrimPrefix(key, prefix)
		if i := strings.Index(rel, "/"); i >= 0 {
			if dir := prefix + rel[:i+1]; !dirs[dir] {
				dirs[dir] = true
				ois = append(ois, ObjectInfo{Key: dir})
			}
			continue
		}
		ois = append(ois, ObjectInfo{Key: key, Size: v.Size, CreatedAt: v.ModifiedAt, ModifiedAt: v.ModifiedAt})
	}
	if !existed {
		return nil, stores.ErrNotFound
	}
	sort.Slice(ois, func(i, j int) bool { return ois[i].Key < ois[j].Key })
	return ois, nil
}
