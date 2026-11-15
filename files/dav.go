package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/mike76-dev/sombrero/client"
	"github.com/mike76-dev/sombrero/stores"
	"go.sia.tech/renterd/v2/api"
	"golang.org/x/net/webdav"
)

// WebDAV serves the shares as a drive that an operating system mounts over
// HTTP, with the share credentials sent as HTTP Basic authentication: the
// shares are the folders under /dav, and the rights are the SMB rights.

// davPrefix is where the server mounts the WebDAV handler.
const davPrefix = "/dav"

// davSession is the context key the authenticated session travels under,
// since the file system interface knows nothing of requests.
type davSession struct{}

// DAV returns the WebDAV handler, which the server mounts at /dav.
func (h *Handler) DAV() http.Handler {
	wd := &webdav.Handler{
		Prefix:     davPrefix,
		FileSystem: davFS{h: h},
		LockSystem: webdav.NewMemLS(),
		Logger: func(req *http.Request, err error) {
			if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
				log.Printf("WebDAV: %s %s: %v", req.Method, req.URL.Path, err)
			}
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s, ok := h.basicAuth(req)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="Sombrero"`)
			writeError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		wd.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), davSession{}, s)))
	})
}

// basicAuth reads the share credentials from the request: the username with
// the workgroup as workgroup\username, workgroup/username or
// username@workgroup, and the password, empty for a guest.
func (h *Handler) basicAuth(req *http.Request) (session, bool) {
	user, password, ok := req.BasicAuth()
	if !ok {
		return session{}, false
	}
	var workgroup, username string
	switch {
	case strings.Contains(user, `\`):
		workgroup, username, _ = strings.Cut(user, `\`)
	case strings.Contains(user, "/"):
		workgroup, username, _ = strings.Cut(user, "/")
	case strings.Contains(user, "@"):
		username, workgroup, _ = strings.Cut(user, "@")
	}
	if workgroup == "" || username == "" {
		return session{}, false
	}

	wg, err := h.resolveWorkgroup(workgroup)
	if err != nil {
		log.Printf("WebDAV: failed to find workgroup: %v", err)
		return session{}, false
	}
	if wg.ID == 0 {
		return session{}, false
	}
	acc, err := h.store.FindAccount(username, wg.UUID.String())
	if err != nil {
		if !errors.Is(err, stores.ErrAccountNotFound) {
			log.Printf("WebDAV: failed to find account: %v", err)
		}
		return session{}, false
	}
	if !acc.CheckPassword(password) {
		return session{}, false
	}
	return session{account: acc, workgroup: wg, expires: h.now().Add(sessionLifetime)}, true
}

// davFS is the shares as a file system: the root holds a folder per share
// the account may read, and each share its files and folders, which the
// account changes with the rights it holds over SMB.
type davFS struct {
	h *Handler
}

// place is where a WebDAV path points: the root, a share, or a file or
// folder on it, with the client that reaches the share and the rights the
// account holds on the share.
type place struct {
	share    stores.Share
	key      string
	client   client.Client
	partSize uint64
	session  session
	rights   Rights
}

// writable says whether the account may write at the key: the share's policy
// and the folder's rights both have to allow it.
func (p place) writable(ctx context.Context) error {
	if p.client == nil || p.key == "" || !p.rights.Write {
		return os.ErrPermission
	}
	return p.allows(ctx, func(pa stores.PathAccess) bool { return pa.Write })
}

// deletable says whether the account may delete at the key.
func (p place) deletable(ctx context.Context) error {
	if p.client == nil || p.key == "" || !p.rights.Delete {
		return os.ErrPermission
	}
	return p.allows(ctx, func(pa stores.PathAccess) bool { return pa.Delete })
}

func (p place) allows(ctx context.Context, ok func(stores.PathAccess) bool) error {
	ac, checks := p.client.(client.AccessChecker)
	if !checks {
		return nil
	}
	pa, err := ac.PathAccess(ctx, p.session.account, p.key)
	if errors.Is(err, stores.ErrNotFound) {
		return os.ErrNotExist
	} else if err != nil {
		return err
	}
	if !ok(pa) {
		return os.ErrPermission
	}
	return nil
}

// Mkdir makes a folder where the account may write.
func (fs davFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	p, _, err := fs.resolve(ctx, name)
	if err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	p, err = fs.placeOf(ctx, name)
	if err != nil {
		return err
	}
	if err := p.writable(ctx); err != nil {
		return err
	}
	err = p.client.MakeDirectory(ctx, p.session.account, p.key)
	switch {
	case errors.Is(err, stores.ErrDirectoryExists):
		return os.ErrExist
	case errors.Is(err, stores.ErrNotFound):
		return os.ErrNotExist
	case errors.Is(err, stores.ErrAccessDenied):
		return os.ErrPermission
	}
	return err
}

// RemoveAll deletes a file, or a folder with everything in it, where the
// account may delete. A folder's files go one by one, so that the share keeps
// each as it keeps any deleted file.
func (fs davFS) RemoveAll(ctx context.Context, name string) error {
	p, info, err := fs.resolve(ctx, name)
	if err != nil {
		return err
	}
	if err := p.deletable(ctx); err != nil {
		return err
	}
	if info.dir {
		if err := deleteFiles(ctx, p.client, p.session.account, p.key); err != nil {
			return err
		}
	}
	err = p.client.Delete(ctx, p.session.account, p.key, info.dir)
	switch {
	case errors.Is(err, stores.ErrNotFound):
		return os.ErrNotExist
	case errors.Is(err, stores.ErrAccessDenied):
		return os.ErrPermission
	}
	return err
}

// Rename moves a file or a folder within its share, where the account may
// delete it and write at the destination.
func (fs davFS) Rename(ctx context.Context, oldName, newName string) error {
	from, info, err := fs.resolve(ctx, oldName)
	if err != nil {
		return err
	}
	to, err := fs.placeOf(ctx, newName)
	if err != nil {
		return err
	}
	if to.share.Name != from.share.Name || to.key == "" || from.key == "" {
		return os.ErrPermission
	}
	if err := from.deletable(ctx); err != nil {
		return err
	}
	if err := to.writable(ctx); err != nil {
		return err
	}
	err = from.client.Rename(ctx, from.session.account, from.key, to.key, info.dir, false)
	switch {
	case errors.Is(err, stores.ErrNotFound):
		return os.ErrNotExist
	case errors.Is(err, stores.ErrAccessDenied):
		return os.ErrPermission
	case errors.Is(err, stores.ErrDirectoryExists):
		return os.ErrExist
	}
	return err
}

// placeOf is the place a path names on its share, whether or not anything is
// there yet.
func (fs davFS) placeOf(ctx context.Context, name string) (place, error) {
	s, ok := ctx.Value(davSession{}).(session)
	if !ok {
		return place{}, os.ErrPermission
	}
	name = path.Clean("/" + name)
	if name == "/" {
		return place{session: s}, nil
	}
	shareName, rest, _ := strings.Cut(strings.TrimPrefix(name, "/"), "/")
	share, err := fs.h.store.GetShare(shareName)
	if err != nil {
		return place{}, err
	}
	if share.Name == "" {
		return place{}, os.ErrNotExist
	}
	r, err := fs.h.rightsOn(s.account, share)
	if err != nil {
		return place{}, err
	}
	if !r.Read {
		return place{}, os.ErrNotExist
	}
	c, partSize, err := fs.h.server.ShareClient(share, s.account.Workgroup)
	if err != nil {
		return place{}, err
	}
	return place{share: share, key: rest, client: c, partSize: partSize, session: s, rights: r}, nil
}

// resolve finds what the path names, as far as the account may see it. The
// root and a share need no lookup beyond the rights; anything deeper is
// looked up on the share.
func (fs davFS) resolve(ctx context.Context, name string) (place, davInfo, error) {
	p, err := fs.placeOf(ctx, name)
	if err != nil {
		return place{}, davInfo{}, err
	}
	if p.client == nil {
		return p, davInfo{name: "/", dir: true, modified: fs.h.now()}, nil
	}
	if p.key == "" {
		return p, davInfo{name: p.share.Name, dir: true, modified: fs.h.now()}, nil
	}

	oi, err := p.client.Object(ctx, p.session.account, p.key)
	if errors.Is(err, stores.ErrNotFound) {
		return place{}, davInfo{}, os.ErrNotExist
	} else if err != nil {
		return place{}, davInfo{}, err
	}
	return p, infoOf(oi), nil
}

// infoOf describes a listed object the way the file system does.
func infoOf(oi client.ObjectInfo) davInfo {
	key := strings.TrimSuffix(oi.Key, "/")
	return davInfo{name: path.Base(key), dir: strings.HasSuffix(oi.Key, "/"), size: oi.Size, modified: oi.ModifiedAt}
}

func (fs davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	_, info, err := fs.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// OpenFile opens a file for reading, a folder for listing, or a file for
// writing where the account may write, which stores what is written over
// whatever was there.
func (fs davFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		if flag&os.O_APPEND != 0 {
			return nil, os.ErrPermission
		}
		p, err := fs.placeOf(ctx, name)
		if err != nil {
			return nil, err
		}
		if err := p.writable(ctx); err != nil {
			return nil, err
		}
		if oi, err := p.client.Object(ctx, p.session.account, p.key); err == nil && strings.HasSuffix(oi.Key, "/") {
			return nil, os.ErrPermission
		}
		return &davWriter{ctx: ctx, h: fs.h, place: p, buf: make([]byte, 0, p.partSize)}, nil
	}
	p, info, err := fs.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	f := &davFile{fs: fs, place: p, info: info}
	if !info.dir {
		f.reader = &objectReader{
			fetch: func(offset, length uint64, w io.Writer) error {
				return p.client.Read(ctx, p.session.account, p.key, offset, length, w)
			},
			path:  p.key,
			size:  info.size,
			chunk: fs.h.chunkSize(p.partSize),
		}
	}
	return f, nil
}

// davInfo is what the file system says about a file or a folder.
type davInfo struct {
	name     string
	dir      bool
	size     uint64
	modified time.Time
}

func (i davInfo) Name() string       { return i.name }
func (i davInfo) Size() int64        { return int64(i.size) }
func (i davInfo) ModTime() time.Time { return i.modified }
func (i davInfo) IsDir() bool        { return i.dir }
func (i davInfo) Sys() any           { return nil }

func (i davInfo) Mode() os.FileMode {
	if i.dir {
		return os.ModeDir | 0o555
	}
	return 0o444
}

// ContentType names the type by the extension, so that a listing does not
// read the head of every file to find out.
func (i davInfo) ContentType(context.Context) (string, error) {
	if i.dir {
		return "httpd/unix-directory", nil
	}
	if ctype := mime.TypeByExtension(path.Ext(i.name)); ctype != "" {
		return ctype, nil
	}
	return "application/octet-stream", nil
}

// davFile is an open file or folder.
type davFile struct {
	fs     davFS
	place  place
	info   davInfo
	reader *objectReader

	children []os.FileInfo
	listed   bool
}

func (f *davFile) Close() error                       { return nil }
func (f *davFile) Stat() (os.FileInfo, error)         { return f.info, nil }
func (f *davFile) Write([]byte) (int, error)          { return 0, os.ErrPermission }
func (f *davFile) Seek(o int64, w int) (int64, error) { return f.readerOf().Seek(o, w) }
func (f *davFile) Read(p []byte) (int, error)         { return f.readerOf().Read(p) }

// readerOf is the file's reader, or one that reads nothing for a folder.
func (f *davFile) readerOf() io.ReadSeeker {
	if f.reader == nil {
		return strings.NewReader("")
	}
	return f.reader
}

// Readdir lists a folder: the shares at the root, the entries of a folder
// otherwise, in the batches the caller asks for.
func (f *davFile) Readdir(count int) ([]os.FileInfo, error) {
	if !f.info.dir {
		return nil, errors.New("not a folder")
	}
	if !f.listed {
		children, err := f.list()
		if err != nil {
			return nil, err
		}
		f.children, f.listed = children, true
	}
	if count <= 0 {
		all := f.children
		f.children = nil
		return all, nil
	}
	if len(f.children) == 0 {
		return nil, io.EOF
	}
	n := min(count, len(f.children))
	batch := f.children[:n]
	f.children = f.children[n:]
	return batch, nil
}

// davWriter takes what a client uploads and stores it as the file, a part at
// a time, the way the file API's upload does; closing it finishes the file.
// Nothing empty can be stored, so an empty upload stores nothing.
type davWriter struct {
	ctx   context.Context
	h     *Handler
	place place

	buf    []byte
	id     string
	number int
	offset uint64
	parts  []api.MultipartCompletedPart
	failed error
}

func (w *davWriter) Read([]byte) (int, error)           { return 0, os.ErrPermission }
func (w *davWriter) Seek(int64, int) (int64, error)     { return 0, os.ErrPermission }
func (w *davWriter) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrPermission }

func (w *davWriter) Stat() (os.FileInfo, error) {
	return davInfo{name: path.Base(w.place.key), size: w.offset + uint64(len(w.buf)), modified: w.h.now()}, nil
}

// Write gathers the bytes into parts of the share's size and sends each as
// it fills.
func (w *davWriter) Write(p []byte) (int, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	written := 0
	for len(p) > 0 {
		room := cap(w.buf) - len(w.buf)
		n := min(room, len(p))
		w.buf = append(w.buf, p[:n]...)
		p, written = p[n:], written+n
		if len(w.buf) == cap(w.buf) {
			if err := w.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// flush sends the gathered part, starting the upload with the first one.
func (w *davWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	c, acc := w.place.client, w.place.session.account
	if w.id == "" {
		id, err := c.StartUpload(w.ctx, acc, w.place.key)
		if err != nil {
			return w.fail(err)
		}
		w.id = id
	}
	w.number++
	eTag, err := c.Write(w.ctx, bytes.NewReader(w.buf), w.place.key, w.id, w.number, w.offset, uint64(len(w.buf)))
	if err != nil {
		return w.fail(err)
	}
	w.parts = append(w.parts, api.MultipartCompletedPart{PartNumber: w.number, ETag: eTag})
	w.offset += uint64(len(w.buf))
	w.buf = w.buf[:0]
	return nil
}

// fail abandons the upload and remembers why.
func (w *davWriter) fail(err error) error {
	if w.id != "" {
		w.h.abortUpload(w.ctx, w.place.client, w.place.key, w.id)
		w.id = ""
	}
	w.failed = err
	return err
}

// Close finishes the file with what was written.
func (w *davWriter) Close() error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.flush(); err != nil {
		return err
	}
	if w.id == "" {
		return nil
	}
	if err := w.place.client.FinishUpload(w.ctx, w.place.key, w.id, w.parts); err != nil {
		return w.fail(err)
	}
	w.id = ""
	return nil
}

func (f *davFile) list() ([]os.FileInfo, error) {
	ctx := context.Background()
	s := f.place.session
	if f.place.client == nil {
		shares, err := f.fs.h.store.GetShares(s.account)
		if err != nil {
			return nil, err
		}
		var infos []os.FileInfo
		for _, share := range shares {
			r, err := f.fs.h.rightsOn(s.account, share)
			if err != nil {
				return nil, err
			}
			if r.Read {
				infos = append(infos, davInfo{name: share.Name, dir: true, modified: f.fs.h.now()})
			}
		}
		return infos, nil
	}

	prefix := ""
	if f.place.key != "" {
		prefix = f.place.key + "/"
	}
	ois, err := f.place.client.List(ctx, s.account, prefix)
	if err != nil {
		return nil, err
	}
	infos := make([]os.FileInfo, 0, len(ois))
	for _, oi := range ois {
		if info := infoOf(oi); info.name != "" && info.name != "/" && info.name != "." {
			infos = append(infos, info)
		}
	}
	return infos, nil
}
